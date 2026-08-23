// Package lru is a small generic LRU cache with optional per-entry expiry. It is
// shared rather than duplicated: the Bitcoin Core client bounds its block caches
// with it, i18n bounds the chat→language map, and the Mini App bounds its
// rendered-card cache.
package lru

import "container/list"
import "time"

// Cache is safe for concurrent use only when the caller serialises access — each
// user of it already holds a mutex of its own. Note Get is a *write*: it drops
// an entry it finds expired, so it needs the same lock Put does.
type Cache[K comparable, V any] struct {
    capacity int
    items    map[K]*list.Element
    lru      *list.List
}

// expires is zero for an entry that never goes stale on its own, which is what
// Put stores; only PutTTL sets it.
type entry[K comparable, V any] struct {
    key     K
    value   V
    expires time.Time
}

func New[K comparable, V any](capacity int) *Cache[K, V] {
    return &Cache[K, V]{
        capacity: capacity,
        items:    make(map[K]*list.Element),
        lru:      list.New(),
    }
}

// Get reports a miss for an entry past its expiry, and drops it on the way out.
// Expiry is checked here and nowhere else: nothing sweeps in the background, so
// an entry nobody asks for simply sits until eviction reclaims it.
func (c *Cache[K, V]) Get(key K) (V, bool) {
    if el, ok := c.items[key]; ok {
        var e = el.Value.(*entry[K, V])
        if e.expires.IsZero() || time.Now().Before(e.expires) {
            c.lru.MoveToFront(el)
            return e.value, true
        }
        c.lru.Remove(el)
        delete(c.items, key)
    }
    var zero V
    return zero, false
}

// Put stores an entry that stays until it is evicted, deleted or replaced.
func (c *Cache[K, V]) Put(key K, value V) { c.put(key, value, time.Time{}) }

// PutTTL stores an entry that Get stops returning once ttl has passed.
func (c *Cache[K, V]) PutTTL(key K, value V, ttl time.Duration) {
    c.put(key, value, time.Now().Add(ttl))
}

func (c *Cache[K, V]) put(key K, value V, expires time.Time) {
    if el, ok := c.items[key]; ok {
        c.lru.MoveToFront(el)
        var e = el.Value.(*entry[K, V])
        e.value, e.expires = value, expires
        return
    }
    var el = c.lru.PushFront(&entry[K, V]{key, value, expires})
    c.items[key] = el
    if c.lru.Len() > c.capacity {
        var oldest = c.lru.Back()
        if oldest != nil {
            c.lru.Remove(oldest)
            delete(c.items, oldest.Value.(*entry[K, V]).key)
        }
    }
}

// Delete drops one entry, for a caller that knows it has gone stale.
func (c *Cache[K, V]) Delete(key K) {
    if el, ok := c.items[key]; ok {
        c.lru.Remove(el)
        delete(c.items, key)
    }
}

// Clear drops every entry, for a cache whose whole contents go stale at once.
func (c *Cache[K, V]) Clear() {
    c.items = make(map[K]*list.Element)
    c.lru.Init()
}
