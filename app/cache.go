package app

import "bytes"
import "net/http"
import "sync"
import "time"
import "bitnsbot/logging"
import "bitnsbot/lru"

// cacheTTL is the longest a rendered card is served from memory. Nothing sweeps
// on a timer: a read past the deadline drops the entry and re-renders, which
// gives the same "cleared every ten minutes" behaviour without a goroutine to
// start and stop alongside the server. It backstops the event-driven clearing —
// the block collector, for one, fills the blocks bucket without announcing it.
var cacheTTL = 10 * time.Minute

// blocksCached is how many pages of the block list are kept. A reader pages down
// a few screens and stops, so twenty covers the depth anyone reaches, while
// bounding what an edited page= parameter can make the process hold.
const blocksCached = 20

// cardCache is one card's rendered HTML and when it was rendered.
type cardCache struct {
    html []byte
    at   time.Time
}

var cacheMu sync.Mutex
var feesCache, networkCache, marketCache cardCache
var blocksCache = lru.New[int, []byte](blocksCached)

// pageCache is the whole shell page. One copy serves everyone, which is only
// sound because / carries no per-user data — the rule that already keeps it
// servable without a signature at all. Anything per-chat must stay behind the
// requireInitData endpoints, and caching here is a second reason why.
var pageCache cardCache

// blocksClearedAt is when the whole block cache was last dropped. One timestamp
// for every page: they all go stale together, on the same new block.
var blocksClearedAt time.Time

// invalidate drops one card's rendered HTML. Notify calls it *before* announcing
// the event, so a page reacting immediately cannot be handed the very copy it
// was told to replace.
func invalidate(event string) {
    cacheMu.Lock()
    defer cacheMu.Unlock()
    switch event {
    case "fees":
        feesCache = cardCache{}
    case "network":
        networkCache = cardCache{}
    case "market":
        marketCache = cardCache{}
    case "blocks":
        blocksCache.Clear()
        blocksClearedAt = time.Now()
    default:
        return
    }
    // The page embeds every card, so whichever one moved, the page it would
    // serve to the next visitor is stale.
    pageCache = cardCache{}
}

// resetCache empties every cache, for tests that share this package state. It
// clears the page itself rather than relying on invalidate's side effect, so a
// change to how the page is invalidated cannot quietly leak one test's fixture
// into the next.
func resetCache() {
    for _, e := range []string{"fees", "network", "market", "blocks"} {
        invalidate(e)
    }
    cacheMu.Lock()
    pageCache = cardCache{}
    cacheMu.Unlock()
}

// card writes one single-valued card, rendering it only when nothing is cached
// or the cache has aged out. data is called outside cacheMu because it reaches
// into main's Source, which holds locks of its own; two concurrent misses simply
// render the same bytes twice, which is cheaper than serialising every request
// behind one mutex.
func card(w http.ResponseWriter, c *cardCache, block string, data func() any) {
    cacheMu.Lock()
    var b = c.html
    if time.Since(c.at) >= cacheTTL { b = nil }
    cacheMu.Unlock()
    if b == nil {
        var ok bool
        b, ok = execute(block, data())
        if ok {
            cacheMu.Lock()
            c.html, c.at = b, time.Now()
            cacheMu.Unlock()
        }
    }
    write(w, b)
}

// blocksPage writes one page of the block list, keyed by page number. Deep pages
// cost more to build than shallow ones — Source walks the bucket from the tip —
// so caching per page is what keeps paging back and forth cheap.
func blocksPage(w http.ResponseWriter, page int, data func() any) {
    cacheMu.Lock()
    if time.Since(blocksClearedAt) >= cacheTTL {
        blocksCache.Clear()
        blocksClearedAt = time.Now()
    }
    var b, hit = blocksCache.Get(page)
    cacheMu.Unlock()
    if !hit {
        var ok bool
        b, ok = execute("blocks", data())
        if ok {
            cacheMu.Lock()
            blocksCache.Put(page, b)
            cacheMu.Unlock()
        }
    }
    write(w, b)
}

// execute renders one of the template's card blocks to bytes so the result can
// be cached and written to many responses. A failed render reports false and is
// not cached: caching a half-rendered card would persist the breakage for the
// life of the entry.
func execute(name string, data any) ([]byte, bool) {
    var buf bytes.Buffer
    var err = appTmpl.ExecuteTemplate(&buf, name, data)
    if err != nil { logging.Err("mini app: render %s: %v", name, err) }
    return buf.Bytes(), err == nil
}

func write(w http.ResponseWriter, b []byte) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Write(b)
}
