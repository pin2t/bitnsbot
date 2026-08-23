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

func (s *countingSource) BlockInfo(height int64) Info {
    s.hit("blockinfo" + strconv.FormatInt(height, 10))
    return Info{OK: true, Title: "Block " + strconv.FormatInt(height, 10),
        Rows: []Field{{Label: "Hash", Value: "0000ab...9f21cd"}}}
}

func (s *countingSource) TxInfo(txid string) Info {
    s.hit("txinfo")
    return Info{OK: true, Title: "Transaction " + txid, Rows: []Field{{Label: "Amount", Value: "1 sat"}}}
}

func (s *countingSource) AddrInfo(addr string) Info {
    s.hit("addrinfo")
    return Info{OK: true, Title: "Address " + addr, Rows: []Field{{Label: "Type", Value: "segwit (bech32)"}}}
}

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
    for _, c := range []struct{ path, name string }{
        {"/fees", "fees"}, {"/network", "network"}, {"/market", "market"}} {
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
    var paths = []string{"/fees", "/network", "/market", "/blocks?page=0"}
    for _, p := range paths { get(h, p, data) }
    Notify("fees")
    for _, p := range paths { get(h, p, data) }
    if n := src.count("fees"); n != 2 {
        t.Errorf("fees rendered %d times; the notify should have cleared it", n)
    }
}

// The shell page is cached like the cards it embeds, so repeat visits do not
// re-render the whole template or re-read every Source.
func TestPageServedFromCache(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var first = get(h, "/", "").Body.String()
    var second = get(h, "/", "").Body.String()
    if first != second {
        t.Error("the page changed between two visits with no data behind it moving")
    }
    if !strings.HasSuffix(strings.TrimSpace(second), "</html>") {
        t.Errorf("the cached page is truncated; tail: %q", second[max(0, len(second)-120):])
    }
    for _, name := range []string{"fees", "network", "market", "blocks0"} {
        if n := src.count(name); n != 1 {
            t.Errorf("%s asked Source %d times for two page loads, want 1", name, n)
        }
    }
}

// The page embeds every card, so whichever one moved, the page is stale — a
// visitor arriving after a new block must not be served the pre-block shell.
func TestAnyNotifyClearsPage(t *testing.T) {
    for _, event := range []string{"fees", "network", "market", "blocks"} {
        var src = newCounting()
        var h = handler(t, "TESTTOKEN", src)
        get(h, "/", "")
        Notify(event)
        get(h, "/", "")
        if n := src.count("fees"); n != 2 {
            t.Errorf("after Notify(%q) the page rendered %d times, want 2", event, n)
        }
    }
    // an event no card is keyed to must not throw the page away
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    get(h, "/", "")
    Notify("something-else")
    get(h, "/", "")
    if n := src.count("fees"); n != 1 {
        t.Errorf("an unknown event cleared the page (%d renders); only real cards should", n)
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
    var paths = []string{"/network", "/market", "/blocks?page=0"}
    for _, p := range paths { get(h, p, data) }
    time.Sleep(40 * time.Millisecond)
    for _, p := range paths { get(h, p, data) }
    for _, name := range []string{"network", "market", "blocks0"} {
        if n := src.count(name); n != 2 {
            t.Errorf("%s rendered %d times, want 2 — the entry should have aged out", name, n)
        }
    }
    // the page ages out on the same deadline
    var psrc = newCounting()
    var ph = handler(t, "TESTTOKEN", psrc)
    get(ph, "/", "")
    time.Sleep(40 * time.Millisecond)
    get(ph, "/", "")
    if n := psrc.count("fees"); n != 2 {
        t.Errorf("the page rendered %d times, want 2 — it should have aged out too", n)
    }
}

// The page and the fragments are cached independently, so a hit on one is never
// served as the other — / must come back as a whole document and /fees as a bare
// card.
func TestPageAndFragmentCachesAreSeparate(t *testing.T) {
    var h = handler(t, "TESTTOKEN", newCounting())
    var frag = get(h, "/fees", freshInitData("TESTTOKEN")).Body.String()
    var body = get(h, "/", "").Body.String()
    if strings.Contains(frag, "<html>") {
        t.Errorf("/fees returned the whole page: %q", frag[:min(120, len(frag))])
    }
    if !strings.Contains(body, "<html>") || !strings.Contains(body, `id="fees"`) {
        t.Error("/ did not return the whole page")
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
