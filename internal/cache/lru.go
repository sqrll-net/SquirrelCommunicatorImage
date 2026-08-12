package cache

import (
	"container/list"
	"sync"
)

/** entry represents a single cached file: raw bytes and metadata. */
type entry struct {
	key  string
	data []byte
	size int64
}

/** Cache is a size-bounded in-memory LRU store. Eviction is based on insertion order.
 *  Reads use RLock for maximum concurrency; only writes mutate the LRU list. */
type Cache struct {
	mu          sync.RWMutex
	items       map[string]*list.Element
	lruList     *list.List
	maxSize     int64
	currentSize int64
}

/** New creates a Cache with the given maximum size in bytes. */
func New(maxSize int64) *Cache {
	return &Cache{
		items:   make(map[string]*list.Element),
		lruList: list.New(),
		maxSize: maxSize,
	}
}

/** Get retrieves a file from cache. Returns (nil, false) on miss.
 *  Uses RLock -- does NOT update LRU order for maximum read throughput. */
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	elem, ok := c.items[key]
	if !ok {
		return nil, false
	}
	return elem.Value.(*entry).data, true
}

/** Put stores file bytes in cache, evicting old entries if the size limit is exceeded. */
func (c *Cache) Put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	size := int64(len(data))

	// Update existing entry: replace data and move to front
	if elem, ok := c.items[key]; ok {
		c.lruList.MoveToFront(elem)
		ent := elem.Value.(*entry)
		c.currentSize -= ent.size
		ent.data = data
		ent.size = size
		c.currentSize += size
		c.evictLocked()
		return
	}

	// Insert new entry at front
	ent := &entry{key: key, data: data, size: size}
	elem := c.lruList.PushFront(ent)
	c.items[key] = elem
	c.currentSize += size
	c.evictLocked()
}

/** evictLocked removes entries from the back of the LRU list until usage is under maxSize.
 *  Must be called while c.mu is held (write lock). */
func (c *Cache) evictLocked() {
	for c.currentSize > c.maxSize && c.lruList.Len() > 0 {
		elem := c.lruList.Back()
		if elem == nil {
			break
		}
		ent := elem.Value.(*entry)
		c.lruList.Remove(elem)
		delete(c.items, ent.key)
		c.currentSize -= ent.size
	}
}

/** Remove deletes a specific key from cache (used when a file is deleted from disk). */
func (c *Cache) Remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		ent := elem.Value.(*entry)
		c.lruList.Remove(elem)
		delete(c.items, key)
		c.currentSize -= ent.size
	}
}

/** Len returns the number of items currently in cache. */
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lruList.Len()
}

/** CurrentSize returns total bytes currently stored in cache. */
func (c *Cache) CurrentSize() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentSize
}
