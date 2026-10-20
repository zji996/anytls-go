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
	c.items[key] = c.order.PushFront(&lruEntry{key: key, size: int64(size), value: value})
	c.used += int64(size)
	for c.used > c.capacity {
		last := c.order.Back()
		if last == nil {
			break
		}
		entry := last.Value.(*lruEntry)
		c.order.Remove(last)
		delete(c.items, entry.key)
		c.used -= entry.size
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
		c.used -= el.Value.(*lruEntry).size
		c.order.Remove(el)
		delete(c.items, key)
	}
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
