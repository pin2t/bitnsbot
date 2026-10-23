// Package mp keeps the mempool as the bot sees it between the node's own
// reports: the newest arrivals the Mini App's live list shows, the transaction
// count and virtual size, the flow rate counted from the rawtx frames, and the
// summed output amount and fees — all in memory, never persisted.
package mp

import "context"
import "sync"
import "time"
import "bitnsbot/app"
import "bitnsbot/core"
import "bitnsbot/logging"
import "bitnsbot/signals"

// Kept is how many of the newest mempool arrivals the Mini App's live list
// holds. It is what the page opens on, and the page trims itself to the same
// count as new ones are prepended.
const Kept = 100

// goneKept bounds the log of listed transactions a block has mined, which an
// open page reads to drop their rows. A block mines thousands, but only the ones
// still in the list are logged, so this is a few blocks' worth.
const goneKept = 500

// minedWindow is how long after a coinbase the rawtx frames are taken to be the
// block's own transactions rather than new arrivals. Core republishes every
// transaction of a block it connects, coinbase first, on the same topic a
// mempool arrival comes on — so without this each block would flood the list
// with thousands of transactions that have just left the mempool. A burst is
// over in well under a second; the cost of the margin is that an arrival in the
// seconds right after a block is not listed.
var minedWindow = 3 * time.Second

// Tx is one listed arrival. Seq orders the list and is what an open page asks
// for anything newer than; the mined log shares the counter, so one number
// covers both what was added and what was removed.
type Tx struct {
    Seq    int64
    Txid   string
    Amount int64
    At     time.Time
}

var mu sync.Mutex
var list []Tx
var gone []Tx
var seq int64
var minedUntil time.Time

// flowWindow is what the flow rate is averaged over: the arrivals of the last
// window, per second, and its change against the window before.
var flowWindow = time.Minute

// The mempool's transaction count and virtual size as the Mini App's page shows
// them, and the arrival times the flow rate is counted from — all guarded by
// mu. Every rawtx frame moves the counts, and Start replaces them with the
// node's own figures after every block. statsOK says the node has been asked at
// least once, which is what gives the counts a base to move from; flowSince is
// the first arrival seen, which says how long the rate has been watched.
var count int64
var bytes int64
var statsOK bool
var arrivals []time.Time
var flowSince time.Time

// The mempool summary — total output amount and total fees across the whole
// mempool, recomputed every 10 minutes by main and printed by /mempool with a
// tilde prefix to mark them as cached, not fresh. Between recomputes the amount
// moves with every rawtx frame (Record); the fees do not, a raw transaction not
// saying what it pays. Guarded by mu.
var summaryAmount int64
var summaryFee int64
var summaryOK bool

// Record files one rawtx frame: an arrival goes on the top of the list, and a
// transaction a block has just mined comes off it. Either change is announced
// to open pages; app.Notify throttles the event.
//
// The counts and the mempool's total output amount move with it — up for an
// arrival, down for a mined transaction, and not at all for the coinbase, which
// was never in the mempool. An arrival is also recorded for the flow rate, and
// the ones older than the two windows the rate and its change are read over are
// forgotten.
func Record(id string, amount, vsize int64, coinbase bool) {
    if id == "" { return }
    var now = time.Now()
    var sign = int64(1)
    mu.Lock()
    if coinbase { minedUntil = now.Add(minedWindow) }
    if now.Before(minedUntil) {
        removeMined(id)
        sign = -1
        if coinbase { sign = 0 }
    } else {
        seq++
        list = append([]Tx{{Seq: seq, Txid: id, Amount: amount, At: now}}, list...)
        if len(list) > Kept { list = list[:Kept] }
        if flowSince.IsZero() { flowSince = now }
        var old = 0
        for old < len(arrivals) && now.Sub(arrivals[old]) > 2*flowWindow { old++ }
        arrivals = append(arrivals[old:], now)
    }
    count = max(0, count+sign)
    bytes = max(0, bytes+sign*vsize)
    summaryAmount = max(0, summaryAmount+sign*amount)
    mu.Unlock()
    app.Notify("mempool")
}

// removeMined takes a mined transaction off the list, logging it so an open page
// drops its row too. Called with mu held.
func removeMined(id string) {
    for i, e := range list {
        if e.Txid != id { continue }
        list = append(list[:i:i], list[i+1:]...)
        seq++
        gone = append(gone, Tx{Seq: seq, Txid: id})
        if len(gone) > goneKept { gone = gone[len(gone)-goneKept:] }
        return
    }
}

// List is the listed arrivals newer than after, newest first, the ids of the
// listed ones mined since, and the counter as it stands. A negative after is
// the whole list and no mined ids.
//
// More says the page's list has a hole: the arrivals since after overflowed
// what is kept, or so did the mined log, so the page should be handed the whole
// list rather than a prepend that would leave a gap.
func List(after int64) (top int64, txs []Tx, mined []string, more bool) {
    mu.Lock()
    defer mu.Unlock()
    for _, e := range list {
        if e.Seq <= after { break }
        txs = append(txs, e)
    }
    if after >= 0 {
        for _, g := range gone {
            if g.Seq > after { mined = append(mined, g.Txid) }
        }
        var full = len(list) == Kept && list[len(list)-1].Seq > after+1
        var lost = len(gone) == goneKept && gone[0].Seq > after+1
        more = full || lost
    }
    return seq, txs, mined, more
}

// Flow is the flow rate — transactions arriving per second over the last
// flowWindow — and its change against the window before, each with whether it
// has been watched long enough to say: a rate needs a whole window since the
// first arrival, a change two. The arrivals are counted when asked, so a rate
// falls when they stop rather than holding its last figure.
//
// An arrival in the seconds after a block is taken for one of the block's own
// transactions and not counted, so a window with a block in it reads a little
// low.
func Flow(now time.Time) (rate float64, rateOK bool, change float64, changeOK bool) {
    mu.Lock()
    defer mu.Unlock()
    var recent, before int
    for _, at := range arrivals {
        var age = now.Sub(at)
        if age <= flowWindow {
            recent++
        } else if age <= 2*flowWindow {
            before++
        }
    }
    var watched = now.Sub(flowSince)
    rate = float64(recent) / flowWindow.Seconds()
    change = rate - float64(before)/flowWindow.Seconds()
    return rate, !flowSince.IsZero() && watched >= flowWindow, change, !flowSince.IsZero() && watched >= 2*flowWindow
}

// Stats is the transaction count and virtual size, and whether the node has
// been asked for them yet.
func Stats() (txs, size int64, ok bool) {
    mu.Lock()
    defer mu.Unlock()
    return count, bytes, statsOK
}

// Summary is the mempool's total output amount and fees, and whether they have
// been summed yet.
func Summary() (amount, fee int64, ok bool) {
    mu.Lock()
    defer mu.Unlock()
    return summaryAmount, summaryFee, summaryOK
}

// SetSummary replaces the totals with a fresh sum.
func SetSummary(amount, fee int64, ok bool) {
    mu.Lock()
    summaryAmount, summaryFee, summaryOK = amount, fee, ok
    mu.Unlock()
}

// Start reads the mempool's transaction count and size from the node at start,
// every ten minutes, and after every block, replacing what the rawtx frames
// have moved them to since. After a block it first waits out the burst of the
// block's own transactions: those come off the counts as they pass, and an
// arrival inside the burst is not counted, so a figure read during it would be
// corrected twice or not at all. The goroutine isn't stopped on shutdown — like
// the rates updater, the process exits right after.
func Start() {
    if !core.Enabled() { return }
    go func() {
        var wake = signals.Subscribe(signals.Block)
        Refresh()
        var t = time.NewTicker(10 * time.Minute)
        defer t.Stop()
        for {
            select {
            case <-t.C:
            case <-wake:
                mu.Lock()
                var wait = time.Until(minedUntil)
                mu.Unlock()
                time.Sleep(wait)
            }
            Refresh()
        }
    }()
}

// Refresh replaces the counts with the node's and tells open pages.
func Refresh() {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    var info, err = core.GetMempoolInfo(ctx)
    cancel()
    if err != nil {
        logging.Warn("mempool stats: %v", err)
        return
    }
    mu.Lock()
    count, bytes, statsOK = info.Size, info.Bytes, true
    mu.Unlock()
    app.Notify("mempool")
}

// SeedFlow sets when the flow rate began being watched and the arrivals it is
// counted from, for tests outside the package that render the rate.
func SeedFlow(since time.Time, at []time.Time) {
    mu.Lock()
    flowSince, arrivals = since, at
    mu.Unlock()
}

// Reset forgets everything, for tests.
func Reset() {
    mu.Lock()
    list, gone, seq, minedUntil = nil, nil, 0, time.Time{}
    count, bytes, statsOK = 0, 0, false
    arrivals, flowSince = nil, time.Time{}
    summaryAmount, summaryFee, summaryOK = 0, 0, false
    mu.Unlock()
}
