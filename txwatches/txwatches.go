// Package txwatches holds the in-memory list of one-shot transaction-confirmation
// watches. A confirmation is transient (it fires once, when the transaction is
// mined), so nothing here is persisted — a restart simply forgets pending
// confirmations, which is acceptable. Two kinds share the map, told apart by
// addr: a direct /watch <txid> (addr "") and an address-derived follow-up
// registered when a watched address's incoming tx is first seen (addr = that
// address). The messaging itself lives in package main (it needs the bot and
// btcd); this package only owns the state and the Confirms query.
package txwatches

import "sync"
import "time"

type watch struct {
    chatID    int64
    alias     string
    watchedAt time.Time
    addr      string // "" for a direct txid watch; the watched address otherwise
}

var mu sync.Mutex
var pending = map[string][]watch{}

// Add registers a direct transaction watch (/watch <txid>), watchedAt = now.
func Add(txid string, chatID int64, alias string) {
    addEntry(txid, watch{chatID: chatID, alias: alias, watchedAt: time.Now()})
}

// AddAddrConfirm registers a one-shot confirmation watch for a transaction just
// seen paying a watched address, so the chat gets a second message — with how
// long it took — once that transaction is mined. watchedAt is now (first-seen).
func AddAddrConfirm(txid string, chatID int64, addr, alias string) {
    addEntry(txid, watch{chatID: chatID, alias: alias, watchedAt: time.Now(), addr: addr})
}

// addEntry appends a watch, skipping an exact (chat, addr) duplicate so a
// re-broadcast of the same transaction can't queue two confirmation messages.
func addEntry(txid string, w watch) {
    mu.Lock()
    defer mu.Unlock()
    for _, e := range pending[txid] {
        if e.chatID == w.chatID && e.addr == w.addr { return }
    }
    pending[txid] = append(pending[txid], w)
}

// Remove drops this chat's direct watches of txid (not the address-derived
// confirmation watches, which RemoveAddrConfirms handles) and returns the count.
func Remove(txid string, chatID int64) int {
    mu.Lock()
    defer mu.Unlock()
    var kept []watch
    var removed int
    for _, w := range pending[txid] {
        if w.chatID == chatID && w.addr == "" { removed++ } else { kept = append(kept, w) }
    }
    if len(kept) == 0 { delete(pending, txid) } else { pending[txid] = kept }
    return removed
}

// RemoveAddrConfirms drops any pending confirmation watches this chat holds for
// transactions on addr — called when the address itself is unwatched, so a
// confirmation can't arrive after the user stopped watching it.
func RemoveAddrConfirms(addr string, chatID int64) {
    mu.Lock()
    defer mu.Unlock()
    for txid, ws := range pending {
        var kept []watch
        for _, w := range ws {
            if w.chatID == chatID && w.addr == addr { continue }
            kept = append(kept, w)
        }
        if len(kept) == len(ws) { continue }
        if len(kept) == 0 { delete(pending, txid) } else { pending[txid] = kept }
    }
}

// Entry is a direct transaction watch, for the /watches listing.
type Entry struct {
    Txid  string
    Alias string
}

// For returns the direct transaction watches a chat holds (address-derived
// confirmation watches are internal and not listed).
func For(chatID int64) (res []Entry) {
    mu.Lock()
    defer mu.Unlock()
    for txid, ws := range pending {
        for _, w := range ws {
            if w.chatID == chatID && w.addr == "" { res = append(res, Entry{Txid: txid, Alias: w.alias}) }
        }
    }
    return res
}

// Confirmed is a watch whose transaction appeared in a block — everything the
// caller needs to compose the "confirmed" message.
type Confirmed struct {
    Txid      string
    ChatID    int64
    Alias     string
    Addr      string // "" for a direct watch; the watched address otherwise
    WatchedAt time.Time
}

// Any reports whether anything is being watched — a cheap early-out so an idle
// bot does no block fetch.
func Any() bool {
    mu.Lock()
    defer mu.Unlock()
    return len(pending) > 0
}

// Confirms removes and returns every watch whose txid is in txids (the txids of a
// newly connected block), so the caller can message each and they never fire
// twice.
func Confirms(txids []string) (res []Confirmed) {
    mu.Lock()
    defer mu.Unlock()
    for _, txid := range txids {
        ws, ok := pending[txid]
        if !ok { continue }
        for _, w := range ws {
            res = append(res, Confirmed{Txid: txid, ChatID: w.chatID, Alias: w.alias, Addr: w.addr, WatchedAt: w.watchedAt})
        }
        delete(pending, txid)
    }
    return res
}

// Reset clears all watches (used by shutdown and test setup).
func Reset() {
    mu.Lock()
    defer mu.Unlock()
    pending = map[string][]watch{}
}
