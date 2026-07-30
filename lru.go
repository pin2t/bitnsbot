package main

import "container/list"

// lruCache is a small generic LRU cache: safe for concurrent use only when the
// caller serialises access (the block caches are only touched from the caller's
// goroutine or behind the coreClient mutex).
type lruCache[K comparable, V any] struct {
	capacity int
	items    map[K]*list.Element
	lru      *list.List
}

type lruEntry[K comparable, V any] struct {
	key   K
	value V
}

func newLRU[K comparable, V any](capacity int) *lruCache[K, V] {
	return &lruCache[K, V]{
		capacity: capacity,
		items:    make(map[K]*list.Element),
		lru:      list.New(),
	}
}

func (c *lruCache[K, V]) Get(key K) (V, bool) {
	if el, ok := c.items[key]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*lruEntry[K, V]).value, true
	}
	var zero V
	return zero, false
}

func (c *lruCache[K, V]) Put(key K, value V) {
	if el, ok := c.items[key]; ok {
		c.lru.MoveToFront(el)
		el.Value.(*lruEntry[K, V]).value = value
		return
	}
	el := c.lru.PushFront(&lruEntry[K, V]{key, value})
	c.items[key] = el
	if c.lru.Len() > c.capacity {
		oldest := c.lru.Back()
		if oldest != nil {
			c.lru.Remove(oldest)
			delete(c.items, oldest.Value.(*lruEntry[K, V]).key)
		}
	}
}
