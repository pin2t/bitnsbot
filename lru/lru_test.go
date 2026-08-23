package lru

import "testing"
import "time"

// The oldest entry goes when capacity is exceeded, and reading one makes it the
// newest — that is the whole contract callers rely on.
func TestEvictsLeastRecentlyUsed(t *testing.T) {
    var c = New[string, int](2)
    c.Put("a", 1)
    c.Put("b", 2)
    if _, ok := c.Get("a"); !ok {
        t.Fatal("a was dropped while still within capacity")
    }
    // a is now the newest, so b is what the third insert evicts
    c.Put("c", 3)
    if _, ok := c.Get("b"); ok {
        t.Error("b survived; the least recently used entry should have gone")
    }
    for _, k := range []string{"a", "c"} {
        if _, ok := c.Get(k); !ok {
            t.Errorf("%s was evicted; it was more recently used than b", k)
        }
    }
}

// Re-putting a key must replace its value rather than add a second entry, or a
// hot key would evict everything else.
func TestPutReplacesValue(t *testing.T) {
    var c = New[string, int](2)
    c.Put("a", 1)
    c.Put("a", 9)
    c.Put("b", 2)
    if v, ok := c.Get("a"); !ok || v != 9 {
        t.Errorf("a = %d, %v; want 9, true", v, ok)
    }
    if _, ok := c.Get("b"); !ok {
        t.Error("b was evicted, so the repeated key grew the cache instead of replacing")
    }
}

// Clear must empty the list as well as the map. Clearing only the map is not
// observable through Get — the stale entries are unreachable and drain away as
// later inserts evict them — so the list is asserted directly: until it drains,
// it holds every cleared value alive, which for the Mini App is a rendered page
// of HTML per cleared entry.
func TestClearEmptiesCache(t *testing.T) {
    var c = New[string, int](2)
    c.Put("a", 1)
    c.Put("b", 2)
    c.Clear()
    if c.lru.Len() != 0 || len(c.items) != 0 {
        t.Errorf("after Clear: list holds %d, map holds %d; both should be empty — "+
            "cleared values stay referenced until later inserts evict them",
            c.lru.Len(), len(c.items))
    }
    for _, k := range []string{"a", "b"} {
        if _, ok := c.Get(k); ok {
            t.Errorf("%s survived Clear", k)
        }
    }
    c.Put("c", 3)
    c.Put("d", 4)
    for _, k := range []string{"c", "d"} {
        if _, ok := c.Get(k); !ok {
            t.Errorf("%s was evicted after Clear; the cache did not reclaim its capacity", k)
        }
    }
}

// An entry stored with a TTL stops being returned once it passes, and one stored
// without a TTL never does.
func TestPutTTLExpires(t *testing.T) {
    var c = New[string, int](4)
    c.PutTTL("soon", 1, 20*time.Millisecond)
    c.Put("forever", 2)
    if _, ok := c.Get("soon"); !ok {
        t.Fatal("the entry expired before its TTL had passed")
    }
    time.Sleep(40 * time.Millisecond)
    if _, ok := c.Get("soon"); ok {
        t.Error("the entry outlived its TTL")
    }
    if v, ok := c.Get("forever"); !ok || v != 2 {
        t.Errorf("forever = %d, %v; a Put with no TTL must never expire", v, ok)
    }
}

// An expired entry is dropped when it is read, not merely hidden — otherwise a
// cache of expired entries would hold its values alive and evict live ones.
func TestExpiredEntryIsDropped(t *testing.T) {
    var c = New[string, int](2)
    c.PutTTL("a", 1, 10*time.Millisecond)
    time.Sleep(30 * time.Millisecond)
    c.Get("a")
    if c.lru.Len() != 0 || len(c.items) != 0 {
        t.Errorf("after reading an expired entry: list holds %d, map holds %d; want both empty",
            c.lru.Len(), len(c.items))
    }
}

// Re-putting a key resets its expiry, so a refreshed entry gets a full life
// rather than inheriting the old deadline.
func TestPutTTLResetsExpiry(t *testing.T) {
    var c = New[string, int](2)
    c.PutTTL("a", 1, 20*time.Millisecond)
    time.Sleep(15 * time.Millisecond)
    c.PutTTL("a", 2, 20*time.Millisecond)
    time.Sleep(10 * time.Millisecond)
    if v, ok := c.Get("a"); !ok || v != 2 {
        t.Errorf("a = %d, %v; the re-put should have restarted the TTL", v, ok)
    }
}

// Replacing a TTL entry with a plain Put makes it permanent again.
func TestPutClearsExpiry(t *testing.T) {
    var c = New[string, int](2)
    c.PutTTL("a", 1, 10*time.Millisecond)
    c.Put("a", 2)
    time.Sleep(30 * time.Millisecond)
    if v, ok := c.Get("a"); !ok || v != 2 {
        t.Errorf("a = %d, %v; a plain Put must clear the old expiry", v, ok)
    }
}

// Delete removes one entry and leaves the rest — the app clears one stale card
// without flushing the others.
func TestDeleteRemovesOneEntry(t *testing.T) {
    var c = New[string, int](3)
    c.Put("a", 1)
    c.Put("b", 2)
    c.Delete("a")
    c.Delete("missing")
    if _, ok := c.Get("a"); ok {
        t.Error("a survived Delete")
    }
    if _, ok := c.Get("b"); !ok {
        t.Error("b was dropped; Delete must not touch other entries")
    }
    if c.lru.Len() != 1 {
        t.Errorf("list holds %d after deleting one of two, want 1", c.lru.Len())
    }
}
