package main

import "context"
import "fmt"
import "html"
import "math"
import "strconv"
import "slices"
import "strings"
import "sync"
import "time"

import "bitnsbot/logging"
import "bitnsbot/txwatches"
import "bitnsbot/watches"

type watchType string

const watchTypeTransaction watchType = "transaction"
const watchTypeAddress watchType = "address"

type notification struct {
    txid         string
    received     map[string]float64 // address → value paid to it by this tx's outputs
    sent         map[string]float64 // address → value spent from it by this tx's inputs
    fee          float64            // BTC
    feeRate      float64            // sat/vB
    confEstimate string             // "~10-20 min" etc; "" if unavailable
    feeOK        bool               // whether fee/feeRate are populated
}

type notifyKey struct {
    chat int64
    typ  watchType
    id   string
}

type notifyChans struct {
    ch   chan notification
    stop chan struct{}
}

var notifyMu sync.Mutex
var notifies = make(map[notifyKey]notifyChans)

func startNotifyChat(b *bot, chatID int64, typ watchType, watchID, alias string) {
    var ch = make(chan notification)
    var stop = make(chan struct{})
    notifyMu.Lock()
    notifies[notifyKey{chatID, typ, watchID}] = notifyChans{ch, stop}
    notifyMu.Unlock()
    go func(b *bot, chatID int64, typ watchType, watchID, alias string, ch <-chan notification, stop chan struct{}) {
        var label = short(watchID)
        if alias != "" {
            label += " (" + html.EscapeString(alias) + ")"
        }
        for {
            select {
            case <-stop:
                return
            case n := <-ch:
                if typ == watchTypeAddress {
                    var in, gotIn = n.received[watchID]
                    var out, gotOut = n.sent[watchID]
                    if gotIn || gotOut {
                        var header = "🔔 New transaction on watched address "
                        var pairs = [][2]string{{"Tx", short(n.txid)}}
                        if gotOut {
                            // spending from the address: report what left it, and
                            // when some came back as change, the net balance move
                            header = "🔔 Outgoing transaction from watched address "
                            pairs = append(pairs, [2]string{"Sent", amountLine(out, time.Time{}, true)})
                            if gotIn {
                                pairs = append(pairs,
                                    [2]string{"Change back", amountLine(in, time.Time{}, true)},
                                    [2]string{"Net", amountLine(in-out, time.Time{}, true)},
                                )
                            }
                        } else {
                            pairs = append(pairs, [2]string{"Amount", amountLine(in, time.Time{}, true)})
                        }
                        if n.feeOK {
                            pairs = append(pairs,
                                [2]string{"Fee", satoshi(n.fee) + " sats"},
                                [2]string{"Fee rate", strings.TrimSuffix(strconv.FormatFloat(n.feeRate, 'f', 1, 64), ".0") + " sat/vB"},
                            )
                            if n.confEstimate != "" {
                                pairs = append(pairs, [2]string{"Confirms", n.confEstimate})
                            }
                        }
                        var pad int
                        for _, p := range pairs {
                            if len(p[0])+1 > pad { pad = len(p[0]) + 1 }
                        }
                        var lines []string
                        for _, p := range pairs {
                            lines = append(lines, fmt.Sprintf("%-*s %s", pad, p[0]+":", p[1]))
                        }
                        sendLinked(b, chatID, header+label+"\n\n<pre>"+strings.Join(lines, "\n")+"</pre>", []string{n.txid, watchID})
                        txwatches.AddAddrConfirm(n.txid, chatID, watchID, alias)
                    }
                }
            }
        }
    }(b, chatID, typ, watchID, alias, ch, stop)
}

func stopNotifyChat(chat int64, typ watchType, id string) {
    notifyMu.Lock()
    defer notifyMu.Unlock()
    var key = notifyKey{chat, typ, id}
    if c, found := notifies[key]; found {
        close(c.stop)
        delete(notifies, key)
    }
}

func notifyAddresses() (res []string) {
    notifyMu.Lock()
    defer notifyMu.Unlock()
    res = make([]string, 0, len(notifies))
    for k, _ := range notifies {
        if k.typ == watchTypeAddress && !slices.Contains(res, k.id) {
            res = append(res, k.id)
        }
    }
    return res
}

// notified remembers which transactions have already been announced, keyed by
// txid. Core publishes a transaction on the `rawtx` topic **twice** — once when
// it enters the mempool and again when it is mined — so without this a watched
// transaction produces two "New transaction" messages, the second one lacking
// the fee and confirmation estimate because the transaction has left the mempool
// by then. Verified against a real node: sighting #1 reports confirmations=0 and
// is in the mempool, sighting #2 reports confirmations=1 and is not.
//
// Deduplicating on the txid rather than skipping confirmed transactions keeps
// the case where a transaction is first seen *already mined* — a coinbase paying
// a watched address, or any transaction that arrived while the bot was down —
// which should still notify. It also collapses rebroadcasts. Only transactions
// that matched a watch get this far, so the map stays small.
var notifiedMu sync.Mutex
var notified = make(map[string]time.Time)

// notifiedTTL is how long a txid is remembered. It only has to outlive the gap
// between a transaction entering the mempool and being mined.
const notifiedTTL = 48 * time.Hour

func alreadyNotified(txid string) bool {
    notifiedMu.Lock()
    defer notifiedMu.Unlock()
    var _, seen = notified[txid]
    if !seen {
        var cutoff = time.Now().Add(-notifiedTTL)
        for id, at := range notified {
            if at.Before(cutoff) { delete(notified, id) }
        }
        notified[txid] = time.Now()
    }
    return seen
}

func broadcast(txHex string) {
    if core == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var tx, err = core.decodeRawTransaction(ctx, txHex)
    if err != nil {
        logging.Err("decode notified tx: %v", err)
        return
    }
    if alreadyNotified(tx.Txid) {
        logging.Info("skipping already-notified transaction %s", short(tx.Txid))
        return
    }
    var n = notification{txid: tx.Txid, received: make(map[string]float64), sent: make(map[string]float64)}
    for _, vout := range tx.Vout {
        if a := vout.ScriptPubKey.Address; a != "" {
            n.received[a] += vout.Value
        }
    }
    // record the outpoints this transaction creates for watched addresses, so a
    // later spend of them is recognised — the bookkeeping btcd used to do inside
    // its own filter
    recordOutpoints(tx.Txid, tx.Vout)
    // the decoded transaction names only the receiving addresses — its inputs are
    // bare txid:vout refs — so the *sending* addresses need the prevouts, which
    // txInputs gathers. The fee comes from getmempoolentry rather than from the
    // transaction: this is a mempool transaction, so it has no undo data and Core
    // reports neither fee nor prevout for it at verbosity 2.
    if full, ferr := core.getRawTransaction(ctx, tx.Txid); ferr == nil {
        if _, _, spent, ok := txInputs(ctx, full); ok {
            n.sent = spent
        }
        if entry, eerr := core.getMempoolEntry(ctx, tx.Txid); eerr == nil && entry.Vsize > 0 {
            n.fee = entry.Fees.Base
            n.feeRate = math.Round(entry.Fees.Base*1e8) / float64(entry.Vsize)
            n.confEstimate = confEstimate(ctx, n.feeRate)
            n.feeOK = true
        }
    }
    notifyMu.Lock()
    var chans = make([]chan notification, 0)
    for _, v := range notifies {
        chans = append(chans, v.ch)
    }
    notifyMu.Unlock()
    for _, ch := range chans {
        select {
        case ch <- n:
        case <-time.After(500 * time.Millisecond):
        }
    }
}

// seedOutpoints loads each watched address's current unspent outputs into the
// bot's own outpoint set, in the background. This is what makes *outgoing*
// notifications work for coins the bot did not watch arrive: a spend names only
// the outpoint it consumes, never the address, so recognising one means already
// knowing which outpoints the address owns.
//
// Under btcd this seeding fed btcd's own connection-wide filter and had to
// replay the whole address history to derive the unspent set. Core has no such
// filter — the set lives here (see zmq.go) — and it answers the question
// directly with scantxoutset, which takes every address at once and returns the
// live UTXOs. That is both a smaller job and an exact one: no history paging, so
// none of the old "a capped history loses the newest transactions" caveat.
//
// The scan walks the whole UTXO set and takes minutes on mainnet, which is why
// it runs in the background and covers every address in a single pass.
func seedOutpoints(addrs []string) {
    if core == nil || len(addrs) == 0 { return }
    go func() {
        var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Minute)
        defer cancel()
        // map each address's scriptPubKey so a scan hit can be attributed back
        var owner = map[string]string{}
        for _, addr := range addrs {
            var info, err = core.validateAddress(ctx, addr)
            if err != nil || !info.IsValid {
                logging.Warn("seed outpoints: validate %s: %v", short(addr), err)
                continue
            }
            owner[info.ScriptPubKey] = addr
            watchScript(info.ScriptPubKey, addr)
        }
        if len(owner) == 0 { return }
        var result, err = core.scanTxOutSet(ctx, addrs)
        if err != nil {
            logging.Warn("scan UTXO set for %d address(es): %v — spends of existing balances may go unreported", len(addrs), err)
            return
        }
        var seeded int
        for _, u := range result.Unspents {
            if addr, ok := owner[u.ScriptPubKey]; ok {
                watchOutpoint(outpoint{u.Txid, u.Vout}, addr)
                seeded++
            }
        }
        logging.Info("watching %d unspent output(s) across %d address(es)", seeded, len(owner))
    }()
}

// confEstimate maps a transaction's fee rate (sat/vB) to a rough confirmation
// window by comparing it to core's fee estimates (BTC/kvB → sat/vB via ×1e5) for
// the 2- and 6-block targets. Returns "" if the estimator has no data yet.
func confEstimate(ctx context.Context, feeRate float64) string {
    var fast, ferr = core.estimateSmartFee(ctx, 2)
    var medium, merr = core.estimateSmartFee(ctx, 6)
    if ferr != nil || merr != nil || fast <= 0 || medium <= 0 {
        return ""
    }
    switch {
    case feeRate >= fast*1e5:
        return "~10-20 min"
    case feeRate >= medium*1e5:
        return "~1h"
    default:
        return "2h+"
    }
}

// startNotify turns every persisted watchCmd back into a live watcher goroutine
// on startup and, if core is connected, loads the watched addresses into core's
// transaction filter so notifications resume across restarts.
func startNotify(bot *bot) {
    var records, err = watches.List()
    if err != nil {
        logging.Err("list watches: %v", err)
        return
    }
    for _, w := range records {
        startNotifyChat(bot, w.ChatID, watchTypeAddress, w.Address, w.Alias)
    }
    var addrs = notifyAddresses()
    if core != nil && len(addrs) > 0 {
        seedOutpoints(addrs)
    }
}

func stopNotify() {
    notifyMu.Lock()
    for _, v := range notifies {
        close(v.stop)
    }
    notifies = make(map[notifyKey]notifyChans)
    notifyMu.Unlock()
    notifiedMu.Lock()
    notified = make(map[string]time.Time)
    notifiedMu.Unlock()
    txwatches.Reset()
}

// checkConfirmations notifies and drops every transaction watch whose transaction
// appears in the just-connected block. The block's txids come from a light
// getblock (verbosity 1); the fetch is skipped entirely when nothing is watched,
// so an idle bot pays nothing per block. Runs off core's read-loop goroutine
// (spawned by notifier.Handle) since it calls back into core.
func checkConfirmations(b *bot, hash string) {
    if b == nil || core == nil { return }
    if !txwatches.Any() { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    var blk, err = core.getBlockTxids(ctx, hash)
    if err != nil {
        logging.Warn("check confirmations for block %s: %v", short(hash), err)
        return
    }
    for _, c := range txwatches.Confirms(blk.Tx) {
        var elapsed = durationText(time.Since(c.WatchedAt))
        var msg string
        if c.Addr != "" {
            var label = short(c.Addr)
            if c.Alias != "" { label += " (" + html.EscapeString(c.Alias) + ")" }
            msg = fmt.Sprintf("🔔 Transaction %s on watched address %s was confirmed in block #%d after %s", short(c.Txid), label, blk.Height, elapsed)
        } else {
            var label = short(c.Txid)
            if c.Alias != "" { label += " (" + html.EscapeString(c.Alias) + ")" }
            msg = fmt.Sprintf("🔔 Watched transaction %s was confirmed in block #%d after %s", label, blk.Height, elapsed)
        }
        var ids = []string{c.Txid}
        if c.Addr != "" { ids = append(ids, c.Addr) }
        ids = append(ids, strconv.FormatInt(blk.Height, 10))
        sendLinked(b, c.ChatID, msg, ids)
        logging.Info("confirmed watch %s for chat %d in block %d", short(c.Txid), c.ChatID, blk.Height)
    }
}
