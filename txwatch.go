package main

import "context"
import "fmt"
import "html"
import "sync"
import "time"

// txWatches holds one-shot confirmation watches, kept only in memory (a
// confirmation is transient — it fires once, when the transaction is mined). Two
// kinds share the map, told apart by addr: a direct /watch <txid> (addr "") and an
// address-derived follow-up registered when a watched address's incoming tx is
// first seen (addr = that address). Neither is persisted; a restart simply forgets
// pending confirmations, which is acceptable. watchedAt (first-seen) drives the
// reported time-to-confirm.
type txWatch struct {
    chatID    int64
    alias     string
    watchedAt time.Time
    addr      string // "" for a direct /watch <txid>; the watched address for an address-derived confirmation
}

var txWatchMu sync.Mutex
var txWatches = map[string][]txWatch{}

// addTxWatch registers a direct transaction watch (/watch <txid>).
func addTxWatch(txid string, chatID int64, alias string) {
    addTxWatchEntry(txid, txWatch{chatID: chatID, alias: alias, watchedAt: time.Now()})
}

// addAddrConfirm registers a one-shot confirmation watch for a transaction just
// seen paying a watched address, so the chat gets a second message — with how
// long it took — once that transaction is mined. watchedAt is now (first-seen).
func addAddrConfirm(txid string, chatID int64, addr, alias string) {
    addTxWatchEntry(txid, txWatch{chatID: chatID, alias: alias, watchedAt: time.Now(), addr: addr})
}

// addTxWatchEntry appends a watch, skipping an exact (chat, addr) duplicate so a
// re-broadcast of the same transaction can't queue two confirmation messages.
func addTxWatchEntry(txid string, w txWatch) {
    txWatchMu.Lock()
    defer txWatchMu.Unlock()
    for _, e := range txWatches[txid] {
        if e.chatID == w.chatID && e.addr == w.addr { return }
    }
    txWatches[txid] = append(txWatches[txid], w)
}

// removeTxWatch drops this chat's direct watches of txid (not the address-derived
// confirmation watches, which /unwatch <address> handles) and returns the count.
func removeTxWatch(txid string, chatID int64) int {
    txWatchMu.Lock()
    defer txWatchMu.Unlock()
    var kept []txWatch
    var removed int
    for _, w := range txWatches[txid] {
        if w.chatID == chatID && w.addr == "" { removed++ } else { kept = append(kept, w) }
    }
    if len(kept) == 0 { delete(txWatches, txid) } else { txWatches[txid] = kept }
    return removed
}

// removeAddrConfirms drops any pending confirmation watches this chat holds for
// transactions on addr — called when the address itself is unwatched, so a
// confirmation can't arrive after the user stopped watching it.
func removeAddrConfirms(addr string, chatID int64) {
    txWatchMu.Lock()
    defer txWatchMu.Unlock()
    for txid, ws := range txWatches {
        var kept []txWatch
        for _, w := range ws {
            if w.chatID == chatID && w.addr == addr { continue }
            kept = append(kept, w)
        }
        if len(kept) == len(ws) { continue }
        if len(kept) == 0 { delete(txWatches, txid) } else { txWatches[txid] = kept }
    }
}

type txWatchEntry struct {
    txid  string
    alias string
}

// txWatchesFor returns the transactions a chat is watching, for /watches.
func txWatchesFor(chatID int64) (res []txWatchEntry) {
    txWatchMu.Lock()
    defer txWatchMu.Unlock()
    for txid, ws := range txWatches {
        for _, w := range ws {
            if w.chatID == chatID && w.addr == "" { res = append(res, txWatchEntry{txid, w.alias}) }
        }
    }
    return res
}

func resetTxWatches() {
    txWatchMu.Lock()
    defer txWatchMu.Unlock()
    txWatches = map[string][]txWatch{}
}

// checkConfirmations notifies and drops every transaction watch whose transaction
// appears in the just-connected block. The block's txids come from a light
// getblock (verbosity 1); the fetch is skipped entirely when nothing is watched,
// so an idle bot pays nothing per block. Runs off btcd's read-loop goroutine
// (spawned by notifier.Handle) since it calls back into btcd.
func checkConfirmations(b *bot, hash string) {
    if b == nil || btcd == nil { return }
    txWatchMu.Lock()
    var watching = len(txWatches) > 0
    txWatchMu.Unlock()
    if !watching { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    var blk, err = btcd.getBlockTxids(ctx, hash)
    if err != nil {
        logWarn("check confirmations for block %s: %v", short(hash), err)
        return
    }
    var confirmed = map[string][]txWatch{}
    txWatchMu.Lock()
    for _, txid := range blk.Tx {
        if ws, ok := txWatches[txid]; ok {
            confirmed[txid] = ws
            delete(txWatches, txid)
        }
    }
    txWatchMu.Unlock()
    for txid, ws := range confirmed {
        for _, w := range ws {
            var elapsed = durationText(time.Since(w.watchedAt))
            var msg string
            if w.addr != "" {
                var label = short(w.addr)
                if w.alias != "" { label += " (" + html.EscapeString(w.alias) + ")" }
                msg = fmt.Sprintf("🔔 Transaction %s on watched address %s was confirmed in block #%d after %s", short(txid), label, blk.Height, elapsed)
            } else {
                var label = short(txid)
                if w.alias != "" { label += " (" + html.EscapeString(w.alias) + ")" }
                msg = fmt.Sprintf("🔔 Watched transaction %s was confirmed in block #%d after %s", label, blk.Height, elapsed)
            }
            send(b, w.chatID, msg)
            logInfo("confirmed watch %s for chat %d in block %d", short(txid), w.chatID, blk.Height)
        }
    }
}

// durationText renders elapsed time compactly: "45 sec", "12 min", "2 h 5 min".
func durationText(d time.Duration) string {
    switch {
    case d < time.Minute:
        return fmt.Sprintf("%d sec", int(d.Seconds()))
    case d < time.Hour:
        return fmt.Sprintf("%d min", int(d.Minutes()))
    default:
        var h = int(d / time.Hour)
        var m = int(d/time.Minute) % 60
        if m == 0 { return fmt.Sprintf("%d h", h) }
        return fmt.Sprintf("%d h %d min", h, m)
    }
}
