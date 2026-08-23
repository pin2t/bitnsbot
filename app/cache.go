package app

import "bytes"
import "net/http"
import "sync"
import "time"
import "bitnsbot/logging"
import "bitnsbot/lru"

// cacheTTL is the longest a rendered card is served from memory. The expiry
// belongs to the entry, checked when it is next read — nothing sweeps on a
// timer, so there is no goroutine to start and stop alongside the server. It
// backstops the event-driven clearing below: the block collector, for one, fills
// the blocks bucket without announcing it.
var cacheTTL = 10 * time.Minute

// blocksCached is how many pages of the block list are kept. A reader pages down
// a few screens and stops, so twenty covers the depth anyone reaches, while
// bounding what an edited page= parameter can make the process hold.
const blocksCached = 20

// cardsCached is exactly the number of single-valued renders — the three cards
// plus the shell page — so nothing is ever evicted for space. This cache is
// keyed and expiring, not bounded; blocksCache is the one that needs the bound.
const cardsCached = 4

var cacheMu sync.Mutex

// cards holds the single-valued renders, keyed by the template block each comes
// from. "app" is the whole shell page: one copy serves everyone, which is only
// sound because / carries no per-user data — the rule that already keeps it
// servable without a signature at all. Anything per-chat must stay behind the
// requireInitData endpoints, and caching here is a second reason why.
var cards = lru.New[string, []byte](cardsCached)

var blocksCache = lru.New[int, []byte](blocksCached)

// invalidate drops one card's rendered HTML. Notify calls it *before* announcing
// the event, so a page reacting immediately cannot be handed the very copy it
// was told to replace.
func invalidate(event string) {
    cacheMu.Lock()
    defer cacheMu.Unlock()
    switch event {
    case "fees", "network", "market":
        cards.Delete(event)
    case "blocks":
        blocksCache.Clear()
    default:
        return
    }
    // The page embeds every card, so whichever one moved, the page it would
    // serve to the next visitor is stale.
    cards.Delete("app")
}

// resetCache empties every cache, for tests that share this package state.
func resetCache() {
    cacheMu.Lock()
    cards.Clear()
    blocksCache.Clear()
    cacheMu.Unlock()
}

// serve writes a cached render, producing it only on a miss. data is called
// outside cacheMu because it reaches into main's Source, which holds locks of
// its own; two concurrent misses simply render the same bytes twice, which is
// cheaper than serialising every request behind one mutex.
func serve[K comparable](w http.ResponseWriter, c *lru.Cache[K, []byte], key K, block string, data func() any) {
    cacheMu.Lock()
    var b, hit = c.Get(key)
    cacheMu.Unlock()
    if !hit {
        var ok bool
        b, ok = execute(block, data())
        if ok {
            cacheMu.Lock()
            c.PutTTL(key, b, cacheTTL)
            cacheMu.Unlock()
        }
    }
    write(w, b)
}

// card serves one of the single-valued renders, whose cache key is the name of
// the template block it comes from.
func card(w http.ResponseWriter, block string, data func() any) {
    serve(w, cards, block, block, data)
}

// execute renders one of the template's blocks to bytes so the result can be
// cached and written to many responses. A failed render reports false and is not
// cached: caching a half-rendered card would persist the breakage for the life
// of the entry.
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
