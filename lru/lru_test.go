package lru

import "testing"

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
