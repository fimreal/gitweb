package cache

import (
	"sync"
	"time"
)

type entry struct {
	data      []byte
	timestamp time.Time
}

type Cache struct {
	mu         sync.RWMutex
	store      map[string]*entry
	ttl        time.Duration
	maxEntries int
}

func New(ttl time.Duration, maxEntries int) *Cache {
	return &Cache{
		store:      make(map[string]*entry),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.store[key]
	if !ok {
		return nil, false
	}

	if time.Since(e.timestamp) > c.ttl {
		return nil, false
	}

	return e.data, true
}

func (c *Cache) Set(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.store) >= c.maxEntries {
		c.evictOldest()
	}

	c.store[key] = &entry{
		data:      data,
		timestamp: time.Now(),
	}
}

func (c *Cache) Invalidate(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k := range c.store {
		if len(prefix) == 0 || len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.store, k)
		}
	}
}

func (c *Cache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	first := true
	for k, e := range c.store {
		if first || e.timestamp.Before(oldestTime) {
			oldestKey = k
			oldestTime = e.timestamp
			first = false
		}
	}

	if oldestKey != "" {
		delete(c.store, oldestKey)
	}
}
