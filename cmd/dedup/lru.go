package main

import (
	"container/list"
	"crypto/sha256"
	"sync"
)

type digest = [sha256.Size]byte

// lru is a byte-bounded LRU. The encoder keeps sizes only (value nil); the
// decoder keeps the chunk bytes.
type lru struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	order    *list.List
	items    map[digest]*list.Element
}

type lruEntry struct {
	key   digest
	size  int64
	value []byte
	// Encoder only: frames carrying the chunk that have no answer yet, and
	// whether the decoder has read one of them.
	pending int32
	held    bool
}

func newLRU(capacity int64) *lru {
	return &lru{capacity: capacity, order: list.New(), items: make(map[digest]*list.Element)}
}

// put records key; value may be nil when only presence matters.
func (c *lru) put(key digest, size int, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.addLocked(&lruEntry{key: key, size: int64(size), value: value})
}

// claim is the encoder's lookup: it reports whether the decoder holds key.
// Otherwise one more frame is about to carry the chunk, until settle.
func (c *lru) claim(key digest, size int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		entry := el.Value.(*lruEntry)
		if entry.held {
			return true
		}
		entry.pending++
		return false
	}
	c.addLocked(&lruEntry{key: key, size: int64(size), pending: 1})
	return false
}

// settle ends a frame's claims. Read by the decoder, its chunks are held;
// otherwise each is dropped unless held already or still in another frame.
func (c *lru) settle(keys []digest, read bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keys {
		el, ok := c.items[key]
		if !ok {
			continue
		}
		entry := el.Value.(*lruEntry)
		if entry.pending > 0 {
			entry.pending--
		}
		if read {
			entry.held = true
		} else if !entry.held && entry.pending == 0 {
			c.removeLocked(el)
		}
	}
}

func (c *lru) addLocked(entry *lruEntry) {
	c.items[entry.key] = c.order.PushFront(entry)
	c.used += entry.size
	for c.used > c.capacity {
		last := c.order.Back()
		if last == nil {
			break
		}
		c.removeLocked(last)
	}
}

// get returns (value, true) and refreshes the entry when present.
func (c *lru) get(key digest) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry).value, true
}

func (c *lru) remove(key digest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
	}
}

func (c *lru) removeLocked(el *list.Element) {
	entry := el.Value.(*lruEntry)
	c.used -= entry.size
	c.order.Remove(el)
	delete(c.items, entry.key)
}

func (c *lru) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	c.items = make(map[digest]*list.Element)
	c.used = 0
}

func (c *lru) stats() (entries int, used int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.used
}

func sha(b []byte) digest { return sha256.Sum256(b) }
