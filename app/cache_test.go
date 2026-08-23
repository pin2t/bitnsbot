package app

import "strconv"
import "strings"
import "sync"
import "testing"
import "time"

// countingSource records how often each card's data was asked for, which is what
// a cache hit is measured by: the fragment still renders, but Source is not
// touched.
type countingSource struct {
    mu    sync.Mutex
    calls map[string]int
    b     map[int]Blocks
}

func newCounting() *countingSource {
    return &countingSource{calls: map[string]int{}, b: liveBlocks()}
}

func (s *countingSource) count(name string) int {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.calls[name]
}

func (s *countingSource) hit(name string) {
    s.mu.Lock()
    s.calls[name]++
    s.mu.Unlock()
}

func (s *countingSource) Fees() Fees       { s.hit("fees"); return liveFees() }
func (s *countingSource) Network() Network { s.hit("network"); return liveNetwork() }
func (s *countingSource) Market() Market   { s.hit("market"); return liveMarket() }

func (s *countingSource) Blocks(page int) Blocks {
    s.hit("blocks" + strconv.Itoa(page))
    if b, ok := s.b[page]; ok { return b }
    // beyond the fixture, a page that exists but holds nothing recognisable —
    // enough for the eviction test to page deep
    return Blocks{OK: true, Page: page, Num: page + 1, Prev: page - 1, Next: page + 1,
        HasPrev: true, HasNext: true, Rows: []Block{{Height: "h" + strconv.Itoa(page)}}}
}

// A second request for an unchanged card is served from memory: the fragment is
// identical and Source was not consulted again.
func TestCardsServedFromCache(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    for _, c := range []struct{ path, name string }{{"/fees", "fees"}, {"/network", "network"}} {
        var first = get(h, c.path, data).Body.String()
        var second = get(h, c.path, data).Body.String()
        if first != second {
            t.Errorf("%s changed between requests:\n%q\n%q", c.path, first, second)
        }
        if n := src.count(c.name); n != 1 {
            t.Errorf("%s asked Source %d times for two requests, want 1", c.path, n)
        }
        if !strings.Contains(first, "<h2>") {
            t.Errorf("%s rendered nothing useful: %q", c.path, first)
        }
    }
}

// Notify is what makes a card update, so it must drop the rendered copy — and
// only that card's.
func TestNotifyClearsOneCard(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    get(h, "/fees", data)
    get(h, "/network", data)
    get(h, "/blocks?page=0", data)
    Notify("fees")
    get(h, "/fees", data)
    get(h, "/network", data)
    get(h, "/blocks?page=0", data)
    if n := src.count("fees"); n != 2 {
        t.Errorf("fees rendered %d times; the notify should have cleared it", n)
    }
    if n := src.count("network"); n != 1 {
        t.Errorf("network rendered %d times; a fees notify must not clear it", n)
    }
    if n := src.count("blocks0"); n != 1 {
        t.Errorf("blocks rendered %d times; a fees notify must not clear it", n)
    }
}

// A new block invalidates the whole list, not just the page that happened to be
// showing: every page shifts by one when a block arrives.
func TestNotifyBlocksClearsEveryPage(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    get(h, "/blocks?page=0", data)
    get(h, "/blocks?page=1", data)
    Notify("blocks")
    get(h, "/blocks?page=0", data)
    get(h, "/blocks?page=1", data)
    for _, p := range []string{"blocks0", "blocks1"} {
        if n := src.count(p); n != 2 {
            t.Errorf("%s rendered %d times, want 2 — a new block stales every page", p, n)
        }
    }
}

// Pages are cached independently and returned under their own key, so paging
// back to one already seen serves that page and not a neighbour's HTML.
func TestBlocksCachedPerPage(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    var first = get(h, "/blocks?page=0", data).Body.String()
    get(h, "/blocks?page=1", data)
    var again = get(h, "/blocks?page=0", data).Body.String()
    if first != again {
        t.Error("paging back to page 0 did not return page 0's cached HTML")
    }
    if !strings.Contains(again, "963268") || strings.Contains(again, "963256") {
        t.Errorf("page 0 came back holding another page's rows: %q", again)
    }
    if n := src.count("blocks0"); n != 1 {
        t.Errorf("page 0 rendered %d times, want 1", n)
    }
    if n := src.count("blocks1"); n != 1 {
        t.Errorf("page 1 rendered %d times, want 1", n)
    }
}

// The list cache is bounded, so an edited page= parameter cannot make the
// process hold an unbounded number of rendered pages.
func TestBlocksCacheEvictsOldestPage(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    for p := 0; p <= blocksCached; p++ {
        get(h, "/blocks?page="+strconv.Itoa(p), data)
    }
    // page 0 is the least recently used of the blocksCached+1 pages requested
    get(h, "/blocks?page=0", data)
    if n := src.count("blocks0"); n != 2 {
        t.Errorf("page 0 rendered %d times, want 2 — it should have been evicted", n)
    }
    // the most recent page is still held
    get(h, "/blocks?page="+strconv.Itoa(blocksCached), data)
    if n := src.count("blocks" + strconv.Itoa(blocksCached)); n != 1 {
        t.Errorf("the newest page rendered %d times, want 1 — it should still be cached", n)
    }
}

// Nothing sweeps the caches on a timer, so the deadline has to be enforced on
// read. This is the backstop for data that changes without a Notify — the block
// collector fills the bucket in the background without announcing it.
func TestCacheExpires(t *testing.T) {
    var old = cacheTTL
    cacheTTL = 20 * time.Millisecond
    defer func() { cacheTTL = old }()
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    get(h, "/fees", data)
    get(h, "/blocks?page=0", data)
    time.Sleep(40 * time.Millisecond)
    get(h, "/fees", data)
    get(h, "/blocks?page=0", data)
    if n := src.count("fees"); n != 2 {
        t.Errorf("fees rendered %d times, want 2 — the entry should have aged out", n)
    }
    if n := src.count("blocks0"); n != 2 {
        t.Errorf("blocks rendered %d times, want 2 — the entry should have aged out", n)
    }
}

// The page at / renders from Source directly, so it never serves a card the
// caches are still holding from before the data moved.
func TestPageIgnoresCache(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    get(h, "/fees", freshInitData("TESTTOKEN"))
    get(h, "/", "")
    if n := src.count("fees"); n != 2 {
        t.Errorf("the page asked Source %d times, want 2 — / must render live", n)
    }
}

// Concurrent misses must not corrupt the cache or race the LRU, which is only
// safe under cacheMu. Run under -race, this is what pins that.
func TestCacheConcurrentRequests(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    var wg sync.WaitGroup
    for i := 0; i < 20; i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            get(h, "/fees", data)
            get(h, "/network", data)
            get(h, "/blocks?page="+strconv.Itoa(i%3), data)
            if i%5 == 0 { Notify("blocks") }
        }(i)
    }
    wg.Wait()
    if body := get(h, "/fees", data).Body.String(); !strings.Contains(body, "36 552") {
        t.Errorf("fees came back mangled after concurrent access: %q", body)
    }
}
