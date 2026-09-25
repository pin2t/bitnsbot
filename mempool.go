package main

import "context"
import "fmt"
import "sync"
import "time"
import "bitnsbot/app"
import "bitnsbot/core"
import "bitnsbot/logging"

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

// recordMempoolTx files one rawtx frame: an arrival goes on the top of the list,
// and a transaction a block has just mined comes off it. Either change is
// announced to open pages; app.Notify throttles the event.
func recordMempoolTx(raw []byte, tx *parsedTx) {
    var id = txid(raw, tx)
    if id == "" { return }
    var now = time.Now()
    mempoolMu.Lock()
    if tx.coinbase { mempoolMinedUntil = now.Add(mempoolMinedWindow) }
    if now.Before(mempoolMinedUntil) {
        var removed = removeMined(id)
        mempoolMu.Unlock()
        if removed { app.Notify("mempool") }
        return
    }
    mempoolSeq++
    mempoolList = append([]mempoolEntry{{seq: mempoolSeq, txid: id, amount: tx.amount, at: now}}, mempoolList...)
    if len(mempoolList) > mempoolKept { mempoolList = mempoolList[:mempoolKept] }
    mempoolMu.Unlock()
    app.Notify("mempool")
}

// removeMined takes a mined transaction off the list, logging it so an open page
// drops its row too. Called with mempoolMu held.
func removeMined(id string) bool {
    for i, e := range mempoolList {
        if e.txid != id { continue }
        mempoolList = append(mempoolList[:i:i], mempoolList[i+1:]...)
        mempoolSeq++
        mempoolGone = append(mempoolGone, mempoolEntry{seq: mempoolSeq, txid: id})
        if len(mempoolGone) > mempoolGoneKept { mempoolGone = mempoolGone[len(mempoolGone)-mempoolGoneKept:] }
        return true
    }
    return false
}

// for testing
func resetMempool() {
    mempoolMu.Lock()
    mempoolList, mempoolGone, mempoolSeq, mempoolMinedUntil = nil, nil, 0, time.Time{}
    mempoolMu.Unlock()
}

// Mempool backs the Mini App's mempool page. A negative after is the whole page: the
// fields /mempool prints and the list as it stands. Anything else is what an
// open page prepends — the arrivals newer than after, and the listed ones mined
// since, whose rows it drops.
//
// More says the page's list has a hole: the arrivals since after overflowed
// what is kept, or so did the mined log, so the page is handed the whole list
// to replace its own with rather than a prepend that would leave a gap.
func (appSource) Mempool(lang string, after int64) app.Mempool {
    var out = app.Mempool{OK: true}
    if after < 0 {
        out.Rows, out.OK = mempoolFields(lang)
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
    var whole = appSource{}.Mempool(lang, -1)
    whole.More = true
    return whole
}

// mempoolFields are the lines /mempool prints — size, transaction count, flow
// rate and the total its transactions move — read the same way.
func mempoolFields(lang string) ([]app.Field, bool) {
    if !core.Enabled() {
        return []app.Field{{Label: i18nl(lang).String("Bitcoin node connection is not configured")}}, false
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    var info, err = core.GetMempoolInfo(ctx)
    if err != nil {
        logging.Warn("mini app: mempool: %v", err)
        return []app.Field{{Label: i18nl(lang).String("Sorry, something went wrong reading the mempool")}}, false
    }
    var pairs = [][2]string{
        {i18nl(lang).String("Size"),         humSize(info.Bytes, 2, lang)},
        {i18nl(lang).String("Transactions"), group(int64(info.Size))},
    }
    flowMu.Lock()
    var rate, rateOK, change, changeOK = flowRate, flowRateOK, flowChange, flowChangeOK
    flowMu.Unlock()
    if rateOK {
        var fr = i18nl(lang).Sprintf("%.1f tx/sec", rate)
        if changeOK { fr += fmt.Sprintf(" (%+.1f)", change) }
        pairs = append(pairs, [2]string{i18nl(lang).String("Flow rate"), fr})
    }
    summaryMu.Lock()
    var amount, ok = summaryAmount, summaryOK
    summaryMu.Unlock()
    if ok && info.Size > 0 {
        pairs = append(pairs, [2]string{i18nl(lang).String("Total flow"), "~" + btcAmount(amount)})
    }
    return appFields(pairs), true
}
