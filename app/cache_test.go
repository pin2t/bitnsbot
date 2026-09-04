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
    b     []Block
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
func (s *countingSource) Market(lang string) Market { s.hit("market"); return liveMarket() }

func (s *countingSource) BlockInfo(lang string, height int64) Info {
    s.hit("blockinfo" + strconv.FormatInt(height, 10))
    return Info{OK: true, Title: "Block " + strconv.FormatInt(height, 10),
        Rows: []Field{{Label: "Hash", Value: "0000ab...9f21cd"}}}
}

func (s *countingSource) TxInfo(lang, txid string) Info {
    s.hit("txinfo")
    return Info{OK: true, Title: "Transaction " + txid, Rows: []Field{{Label: "Amount", Value: "1 sat"}}}
}

func (s *countingSource) AddrInfo(lang, addr string) Info {
    s.hit("addrinfo")
    return Info{OK: true, Title: "Address " + addr, Rows: []Field{{Label: "Type", Value: "segwit (bech32)"}}}
}

func (s *countingSource) MinerInfo(lang, name string) Info {
    s.hit("minerinfo")
    return Info{OK: true, Title: name, Rows: []Field{{Label: "Blocks mined", Value: "22 blocks"}}}
}

func (s *countingSource) Watching(chat int64, kind, id string) bool { return false }

func (s *countingSource) SetWatch(chat int64, kind, id string, on bool) (bool, error) { return on, nil }

func (s *countingSource) SetAlias(chat int64, kind, id, alias string) (bool, error) { return true, nil }

func (s *countingSource) Watches(chat int64) Watches {
    s.hit("watches")
    return Watches{OK: true}
}

func (s *countingSource) Blocks(lang string, rng Range) Blocks {
    s.hit("blocks" + rangeKey(rng))
    var b = window(s.b, rng)
    if !b.OK && rng.Before > 0 {
        // below the fixture: a batch that exists but holds nothing recognisable
        // — enough for the eviction test to scroll deep
        var h = rng.Before - 1
        b = Blocks{OK: true, Top: h, Next: h, More: true,
            Rows: []Block{{Height: strconv.FormatInt(h, 10), Num: h}}}
    }
    return b
}

// rangeKey names a window the way the tests count them: one key per distinct
// request, so a cache hit shows up as a call that did not happen.
func rangeKey(rng Range) string {
    switch {
    case rng.Before > 0: return "before" + strconv.FormatInt(rng.Before, 10)
    case rng.After > 0:  return "after" + strconv.FormatInt(rng.After, 10)
    case rng.Down > 0:   return "down" + strconv.FormatInt(rng.Down, 10)
    }
    return "top"
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
    var paths = []string{"/fees", "/network", "/market", "/blocks"}
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
    for _, name := range []string{"fees", "network", "market", "blockstop"} {
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

// A new block invalidates the whole list, not just the batch that happened to be
// showing: every batch below it shifts by one when a block arrives.
func TestNotifyBlocksClearsEveryPage(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    get(h, "/blocks", data)
    get(h, "/moreblocks?before=963257", data)
    Notify("blocks")
    get(h, "/blocks", data)
    get(h, "/moreblocks?before=963257", data)
    for _, p := range []string{"blockstop", "blocksbefore963257"} {
        if n := src.count(p); n != 2 {
            t.Errorf("%s rendered %d times, want 2 — a new block stales every batch", p, n)
        }
    }
}

// Batches are cached independently and returned under their own key, so a list
// restored by Back serves its own HTML and not a neighbour's.
func TestBlocksCachedPerPage(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    var first = get(h, "/blocks", data).Body.String()
    get(h, "/moreblocks?before=963257", data)
    var again = get(h, "/blocks", data).Body.String()
    if first != again {
        t.Error("re-opening the list did not return its cached HTML")
    }
    if !strings.Contains(again, "963268") || strings.Contains(again, "963256") {
        t.Errorf("the list came back holding another batch's rows: %q", again)
    }
    if n := src.count("blockstop"); n != 1 {
        t.Errorf("the newest batch rendered %d times, want 1", n)
    }
    if n := src.count("blocksbefore963257"); n != 1 {
        t.Errorf("the second batch rendered %d times, want 1", n)
    }
}

// The list cache is bounded, so an edited before= parameter cannot make the
// process hold an unbounded number of rendered batches.
func TestBlocksCacheEvictsOldestPage(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    get(h, "/blocks", data)
    for i := 0; i < blocksCached; i++ {
        get(h, "/moreblocks?before="+strconv.Itoa(963257-i), data)
    }
    // the list itself is the least recently used of the blocksCached+1 renders
    get(h, "/blocks", data)
    if n := src.count("blockstop"); n != 2 {
        t.Errorf("the newest batch rendered %d times, want 2 — it should have been evicted", n)
    }
    // the most recent batch is still held
    var last = strconv.Itoa(963257 - (blocksCached - 1))
    get(h, "/moreblocks?before="+last, data)
    if n := src.count("blocksbefore" + last); n != 1 {
        t.Errorf("the newest batch rendered %d times, want 1 — it should still be cached", n)
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
    var paths = []string{"/network", "/market", "/blocks"}
    for _, p := range paths { get(h, p, data) }
    time.Sleep(40 * time.Millisecond)
    for _, p := range paths { get(h, p, data) }
    for _, name := range []string{"network", "market", "blockstop"} {
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
            get(h, "/moreblocks?before="+strconv.Itoa(963257-i%3), data)
            if i%5 == 0 { Notify("blocks") }
        }(i)
    }
    wg.Wait()
    if body := get(h, "/fees", data).Body.String(); !strings.Contains(body, "36 552") {
        t.Errorf("fees came back mangled after concurrent access: %q", body)
    }
}
