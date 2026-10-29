package main

import (
	"container/list"
	"crypto/cipher"
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// cache owns the directory of encrypted objects and the in-memory LRU that
// bounds it. It holds the master key, so a locked cache reads and writes
// nothing and the server passes every request through.
type cache struct {
	dir      string
	capacity int64
	reserve  int64 // free space always left on disk

	keyMu sync.RWMutex
	key   []byte // master key, never written to disk or logged

	mu      sync.Mutex
	used    int64
	order   *list.List // most recently used at the front
	items   map[string]*list.Element
	pending int
}

type cacheEntry struct {
	name    string
	size    int64
	mtime   time.Time
	writing bool // a placeholder for room reserved by a write in progress
}

func newCache(dir string, capacity, reserve int64) (*cache, error) {
	c := &cache{dir: dir, capacity: capacity, reserve: reserve, order: list.New(), items: make(map[string]*list.Element)}
	tmp := filepath.Join(dir, tmpDirName)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	// Half-written objects from a previous run are worthless.
	if err := emptyDir(tmp); err != nil {
		return nil, err
	}
	if err := c.rebuild(); err != nil {
		return nil, err
	}
	return c, nil
}

func emptyDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// rebuild restores the LRU from the directory: file size and mtime, oldest
// first, then trims to capacity.
func (c *cache) rebuild() error {
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	type found struct {
		name  string
		size  int64
		mtime time.Time
	}
	var files []found
	for _, e := range ents {
		if !e.Type().IsRegular() || !validObjectName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, found{e.Name(), info.Size(), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range files {
		c.items[f.name] = c.order.PushFront(&cacheEntry{name: f.name, size: f.size, mtime: f.mtime})
		c.used += f.size
	}
	for c.used > c.capacity {
		el := c.order.Back()
		if el == nil {
			break
		}
		c.removeLocked(el)
	}
	return nil
}

// validObjectName reports whether name looks like hex(HMAC-SHA256(K, obj)).
func validObjectName(name string) bool {
	if len(name) != 64 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// setKey installs the master key. A second unlock with a different key is
// refused: rotating the key means deleting the directory and restarting.
func (c *cache) setKey(key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("master key is %d bytes, want 32", len(key))
	}
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	if c.key != nil {
		if hmac.Equal(c.key, key) {
			return nil
		}
		return errors.New("already unlocked with a different key; delete the cache directory and restart to rotate")
	}
	k := make([]byte, len(key))
	copy(k, key)
	if err := lockMemory(k); err != nil {
		log.Printf("blobcache: mlock master key: %s", reason(err))
	}
	c.key = k
	return nil
}

func (c *cache) locked() bool {
	c.keyMu.RLock()
	defer c.keyMu.RUnlock()
	return c.key == nil
}

// wipe forgets the master key at shutdown.
func (c *cache) wipe() {
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	for i := range c.key {
		c.key[i] = 0
	}
	c.key = nil
}

// name maps obj to its file name.
func (c *cache) name(obj string) (string, error) {
	c.keyMu.RLock()
	defer c.keyMu.RUnlock()
	if c.key == nil {
		return "", errLocked
	}
	return objectName(c.key, obj), nil
}

// aeadFor derives K_obj under the lock, so the master key never leaves the
// cache and wipe cannot zero it while it is in use.
func (c *cache) aeadFor(obj string) (cipher.AEAD, error) {
	c.keyMu.RLock()
	defer c.keyMu.RUnlock()
	if c.key == nil {
		return nil, errLocked
	}
	return objectAEAD(c.key, obj)
}

// open opens and validates the cached object. A file that fails to parse or
// decrypt is deleted: it is useless without its key or after corruption.
func (c *cache) open(obj string) (*object, error) {
	name, err := c.name(obj)
	if err != nil {
		return nil, err
	}
	aead, err := c.aeadFor(obj)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(c.dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.forget(name)
		}
		return nil, err
	}
	o, err := openObject(f, name, aead)
	if err != nil {
		f.Close()
		c.dropName(name)
		return nil, err
	}
	c.touch(name)
	return o, nil
}

// touch moves name to the front and, at most hourly, records the access on
// disk for the LRU rebuilt after a restart.
func (c *cache) touch(name string) {
	c.mu.Lock()
	el, ok := c.items[name]
	if !ok {
		c.mu.Unlock()
		return
	}
	ent := el.Value.(*cacheEntry)
	c.order.MoveToFront(el)
	refresh := time.Since(ent.mtime) >= touchInterval
	if refresh {
		ent.mtime = time.Now()
	}
	c.mu.Unlock()
	if refresh {
		now := time.Now()
		if err := os.Chtimes(filepath.Join(c.dir, name), now, now); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("blobcache: touch cache file: %s", reason(err))
		}
	}
}

func (c *cache) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[name]; ok {
		c.removeLocked(el)
	}
}

// dropName deletes a cached file, e.g. one that no longer decrypts.
func (c *cache) dropName(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[name]; ok {
		c.removeLocked(el)
		return
	}
	os.Remove(filepath.Join(c.dir, name))
}

func (c *cache) removeLocked(el *list.Element) {
	ent := el.Value.(*cacheEntry)
	c.used -= ent.size
	c.order.Remove(el)
	delete(c.items, ent.name)
	if !ent.writing {
		if err := os.Remove(filepath.Join(c.dir, ent.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("blobcache: evict: %s", reason(err))
		}
	}
}

// reserve makes room for an object of need bytes and accounts for it while it
// is being written. It evicts oldest-first down to 90% of the capacity and
// refuses when the disk would have less than reserve bytes free afterwards.
func (c *cache) reserveRoom(need int64) (string, error) {
	if need > c.capacity {
		return "", fmt.Errorf("%w: object needs %d bytes, cache holds %d", errNoRoom, need, c.capacity)
	}
	if free, err := diskFree(c.dir); err == nil && free-need < c.reserve {
		return "", fmt.Errorf("%w: %d bytes free, %d needed, %d reserved", errNoRoom, free, need, c.reserve)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used+need > c.capacity {
		target := c.capacity * 9 / 10
		for c.used > target || c.used+need > c.capacity {
			el := c.oldestLocked()
			if el == nil {
				return "", errNoRoom
			}
			c.removeLocked(el)
		}
	}
	c.pending++
	key := fmt.Sprintf("\x00pending-%d", c.pending)
	c.items[key] = c.order.PushFront(&cacheEntry{name: key, size: need, writing: true})
	c.used += need
	return key, nil
}

func (c *cache) oldestLocked() *list.Element {
	for el := c.order.Back(); el != nil; el = el.Prev() {
		if !el.Value.(*cacheEntry).writing {
			return el
		}
	}
	return nil
}

// release gives up a reservation without a file.
func (c *cache) release(pending string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[pending]; ok {
		c.removeLocked(el)
	}
}

// commit turns a reservation into the real entry; size is the file size.
func (c *cache) commit(pending, name string, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[pending]; ok {
		c.used -= el.Value.(*cacheEntry).size
		c.order.Remove(el)
		delete(c.items, pending)
	}
	if el, ok := c.items[name]; ok {
		c.used -= el.Value.(*cacheEntry).size
		c.order.Remove(el)
		delete(c.items, name)
	}
	c.items[name] = c.order.PushFront(&cacheEntry{name: name, size: size, mtime: time.Now()})
	c.used += size
}

func (c *cache) stats() (objects int, used int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items) - c.countWritingLocked(), c.used
}

func (c *cache) countWritingLocked() int {
	n := 0
	for el := c.order.Front(); el != nil; el = el.Next() {
		if el.Value.(*cacheEntry).writing {
			n++
		}
	}
	return n
}
