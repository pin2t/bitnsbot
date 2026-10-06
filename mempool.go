package main

import "context"
import "fmt"
import "sync"
import "time"
import "bitnsbot/app"
import "bitnsbot/core"
import "bitnsbot/logging"
import "bitnsbot/signals"

// mempoolKept is how many of the newest mempool arrivals the Mini App's live
// list holds. It is what the page opens on, and the page trims itself to the
// same count as new ones are prepended.
const mempoolKept = 100

// mempoolGoneKept bounds the log of listed transactions a block has mined, which
// an open page reads to drop their rows. A block mines thousands, but only the
// ones still in the list are logged, so this is a few blocks' worth.
const mempoolGoneKept = 500

// mempoolMinedWindow is how long after a coinbase the rawtx frames are taken to
// be the block's own transactions rather than new arrivals. Core republishes
// every transaction of a block it connects, coinbase first, on the same topic
// a mempool arrival comes on — so without this each block would flood the list
// with thousands of transactions that have just left the mempool. A burst is
// over in well under a second; the cost of the margin is that an arrival in the
// seconds right after a block is not listed.
var mempoolMinedWindow = 3 * time.Second

// mempoolEntry is one listed arrival. seq orders the list and is what an open
// page asks for anything newer than; the mined log shares the counter, so one
// number covers both what was added and what was removed.
type mempoolEntry struct {
    seq    int64
    txid   string
    amount int64
    at     time.Time
}

var mempoolMu sync.Mutex
var mempoolList []mempoolEntry
var mempoolGone []mempoolEntry
var mempoolSeq int64
var mempoolMinedUntil time.Time

// flowWindow is what the flow rate is averaged over: the arrivals of the last
// window, per second, and its change against the window before. A package var
// so tests can shrink it.
var flowWindow = time.Minute

// The mempool's transaction count and virtual size as the Mini App's page shows
// them, and the arrival times the flow rate is counted from — all in memory,
// guarded by mempoolMu. Every rawtx frame moves the counts, and
// startMempoolStats replaces them with the node's own figures after every
// block. mempoolStatsOK says the node has been asked at least once, which is
// what gives the counts a base to move from; flowSince is the first arrival
// seen, which says how long the rate has been watched.
var mempoolCount int64
var mempoolBytes int64
var mempoolStatsOK bool
var mempoolArrivals []time.Time
var flowSince time.Time

// recordMempoolTx files one rawtx frame: an arrival goes on the top of the list,
// and a transaction a block has just mined comes off it. Either change is
// announced to open pages; app.Notify throttles the event.
//
// The counts and the mempool's total output amount move with it — up for an
// arrival, down for a mined transaction, and not at all for the coinbase, which
// was never in the mempool. An arrival is also recorded for the flow rate, and
// the ones older than the two windows the rate and its change are read over are
// forgotten.
func recordMempoolTx(raw []byte, tx *parsedTx) {
    var id = txid(raw, tx)
    if id == "" { return }
    var now = time.Now()
    var sign = int64(1)
    mempoolMu.Lock()
    if tx.coinbase { mempoolMinedUntil = now.Add(mempoolMinedWindow) }
    if now.Before(mempoolMinedUntil) {
        removeMined(id)
        sign = -1
        if tx.coinbase { sign = 0 }
    } else {
        mempoolSeq++
        mempoolList = append([]mempoolEntry{{seq: mempoolSeq, txid: id, amount: tx.amount, at: now}}, mempoolList...)
        if len(mempoolList) > mempoolKept { mempoolList = mempoolList[:mempoolKept] }
        if flowSince.IsZero() { flowSince = now }
        var old = 0
        for old < len(mempoolArrivals) && now.Sub(mempoolArrivals[old]) > 2*flowWindow { old++ }
        mempoolArrivals = append(mempoolArrivals[old:], now)
    }
    mempoolCount = max(0, mempoolCount+sign)
    mempoolBytes = max(0, mempoolBytes+sign*vsize(raw, tx))
    mempoolMu.Unlock()
    summaryMu.Lock()
    summaryAmount = max(0, summaryAmount+sign*tx.amount)
    summaryMu.Unlock()
    app.Notify("mempool")
}

// vsize is a transaction's virtual size, which is what getmempoolinfo's bytes
// sums: its weight — the non-witness bytes four times, the witness once — over
// four, rounded up. A segwit transaction's non-witness bytes are all but its
// marker and flag and the witness between its outputs and its locktime.
func vsize(raw []byte, tx *parsedTx) int64 {
    var base = len(raw)
    if tx.segwit { base = tx.outputsEnd + 2 }
    return int64((base*3 + len(raw) + 3) / 4)
}

// removeMined takes a mined transaction off the list, logging it so an open page
// drops its row too. Called with mempoolMu held.
func removeMined(id string) {
    for i, e := range mempoolList {
        if e.txid != id { continue }
        mempoolList = append(mempoolList[:i:i], mempoolList[i+1:]...)
        mempoolSeq++
        mempoolGone = append(mempoolGone, mempoolEntry{seq: mempoolSeq, txid: id})
        if len(mempoolGone) > mempoolGoneKept { mempoolGone = mempoolGone[len(mempoolGone)-mempoolGoneKept:] }
        return
    }
}

// mempoolFlow is the flow rate — transactions arriving per second over the last
// flowWindow — and its change against the window before, each with whether it
// has been watched long enough to say: a rate needs a whole window since the
// first arrival, a change two. The arrivals are counted when asked, so a rate
// falls when they stop rather than holding its last figure.
//
// An arrival in the seconds after a block is taken for one of the block's own
// transactions and not counted, so a window with a block in it reads a little
// low.
func mempoolFlow(now time.Time) (rate float64, rateOK bool, change float64, changeOK bool) {
    mempoolMu.Lock()
    defer mempoolMu.Unlock()
    var recent, before int
    for _, at := range mempoolArrivals {
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

// startMempoolStats reads the mempool's transaction count and size from the node
// at start, every ten minutes, and after every block, replacing what the rawtx
// frames have moved them to since. After a block it first waits out the burst of
// the block's own transactions: those come off the counts as they pass, and an
// arrival inside the burst is not counted, so a figure read during it would be
// corrected twice or not at all. The goroutine isn't stopped on shutdown — like
// the rates updater, the process exits right after.
func startMempoolStats() {
    if !core.Enabled() { return }
    go func() {
        var wake = signals.Subscribe(signals.Block)
        refreshMempoolStats()
        var t = time.NewTicker(10 * time.Minute)
        defer t.Stop()
        for {
            select {
            case <-t.C:
            case <-wake:
                mempoolMu.Lock()
                var wait = time.Until(mempoolMinedUntil)
                mempoolMu.Unlock()
                time.Sleep(wait)
            }
            refreshMempoolStats()
        }
    }()
}

// refreshMempoolStats replaces the counts with the node's and tells open pages.
func refreshMempoolStats() {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    var info, err = core.GetMempoolInfo(ctx)
    cancel()
    if err != nil {
        logging.Warn("mempool stats: %v", err)
        return
    }
    mempoolMu.Lock()
    mempoolCount, mempoolBytes, mempoolStatsOK = info.Size, info.Bytes, true
    mempoolMu.Unlock()
    app.Notify("mempool")
}

// for testing
func resetMempool() {
    mempoolMu.Lock()
    mempoolList, mempoolGone, mempoolSeq, mempoolMinedUntil = nil, nil, 0, time.Time{}
    mempoolCount, mempoolBytes, mempoolStatsOK = 0, 0, false
    mempoolArrivals, flowSince = nil, time.Time{}
    mempoolMu.Unlock()
    summaryMu.Lock()
    summaryAmount, summaryOK = 0, false
    summaryMu.Unlock()
}

// appMempool backs the Mini App's mempool page. A negative after is the whole page: the
// fields /mempool prints and the list as it stands. Anything else is what an
// open page prepends — the arrivals newer than after, and the listed ones mined
// since, whose rows it drops — with the fields as they stand now, which the page
// swaps in over its own, so its counts move with its list. Fields that cannot
// be read are left out of a prepend rather than replacing good ones with an
// error.
//
// More says the page's list has a hole: the arrivals since after overflowed
// what is kept, or so did the mined log, so the page is handed the whole list
// to replace its own with rather than a prepend that would leave a gap.
func appMempool(lang string, after int64) app.Mempool {
    var out = app.Mempool{OK: true}
    var rows, ok = mempoolFields(lang)
    if after < 0 {
        out.Rows, out.OK = rows, ok
    } else if ok {
        out.Rows = rows
    }
    mempoolMu.Lock()
    out.Top = mempoolSeq
    for _, e := range mempoolList {
        if e.seq <= after { break }
        out.Txs = append(out.Txs, app.MempoolTx{Id: e.txid, Short: short(e.txid),
            Amount: amountText(e.amount, lang), At: e.at.Unix(), Ago: relative(e.at.Unix(), lang)})
    }
    if after >= 0 {
        for _, g := range mempoolGone {
            if g.seq > after { out.Gone = append(out.Gone, g.txid) }
        }
        var full = len(mempoolList) == mempoolKept && mempoolList[len(mempoolList)-1].seq > after+1
        var lost = len(mempoolGone) == mempoolGoneKept && mempoolGone[0].seq > after+1
        out.More = full || lost
    }
    mempoolMu.Unlock()
    if !out.More { return out }
    var whole = appMempool(lang, -1)
    whole.More = true
    return whole
}

// mempoolFields are the lines /mempool prints — size, transaction count, flow
// rate and the total its transactions move — read from memory, where the rawtx
// frames keep them current, so the page asks the node nothing.
func mempoolFields(lang string) ([]app.Field, bool) {
    if !core.Enabled() {
        return []app.Field{{Label: i18nl(lang).String("Bitcoin node connection is not configured")}}, false
    }
    mempoolMu.Lock()
    var count, size, statsOK = mempoolCount, mempoolBytes, mempoolStatsOK
    mempoolMu.Unlock()
    if !statsOK {
        return []app.Field{{Label: i18nl(lang).String("Sorry, something went wrong reading the mempool")}}, false
    }
    var pairs = [][2]string{
        {i18nl(lang).String("Size"),         humSize(size, 2, lang)},
        {i18nl(lang).String("Transactions"), group(count)},
    }
    var rate, rateOK, change, changeOK = mempoolFlow(time.Now())
    if rateOK {
        var fr = i18nl(lang).Sprintf("%.1f tx/sec", rate)
        if changeOK { fr += fmt.Sprintf(" (%+.1f)", change) }
        pairs = append(pairs, [2]string{i18nl(lang).String("Flow rate"), fr})
    }
    summaryMu.Lock()
    var amount, ok = summaryAmount, summaryOK
    summaryMu.Unlock()
    if ok && count > 0 {
        pairs = append(pairs, [2]string{i18nl(lang).String("Total flow"), "~" + btcAmount(amount)})
    }
    return appFields(pairs), true
}
