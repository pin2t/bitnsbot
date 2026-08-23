// Package lru is a small generic LRU cache. It is shared rather than duplicated:
// the Bitcoin Core client bounds its block caches with it, i18n bounds the
// chat→language map, and the Mini App bounds its rendered-page cache.
package lru

import "container/list"

// Cache is safe for concurrent use only when the caller serialises access — each
// user of it already holds a mutex of its own.
type Cache[K comparable, V any] struct {
    capacity int
    items    map[K]*list.Element
    lru      *list.List
}

type entry[K comparable, V any] struct {
    key   K
    value V
}

func New[K comparable, V any](capacity int) *Cache[K, V] {
    return &Cache[K, V]{
        capacity: capacity,
        items:    make(map[K]*list.Element),
        lru:      list.New(),
    }
}

func (c *Cache[K, V]) Get(key K) (V, bool) {
    if el, ok := c.items[key]; ok {
        c.lru.MoveToFront(el)
        return el.Value.(*entry[K, V]).value, true
    }
    var zero V
    return zero, false
}

func (c *Cache[K, V]) Put(key K, value V) {
    if el, ok := c.items[key]; ok {
        c.lru.MoveToFront(el)
        el.Value.(*entry[K, V]).value = value
        return
    }
    var el = c.lru.PushFront(&entry[K, V]{key, value})
    c.items[key] = el
    if c.lru.Len() > c.capacity {
        var oldest = c.lru.Back()
        if oldest != nil {
            c.lru.Remove(oldest)
            delete(c.items, oldest.Value.(*entry[K, V]).key)
        }
    }
}

// Clear drops every entry, for a cache whose whole contents go stale at once.
func (c *Cache[K, V]) Clear() {
    c.items = make(map[K]*list.Element)
    c.lru.Init()
}
