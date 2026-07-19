package main

import "context"
import "encoding/json"
import "fmt"
import "html"
import "math"
import "slices"
import "strconv"
import "strings"
import "sync"
import "time"

import "github.com/sourcegraph/jsonrpc2"
import "bitnsbot/logging"
import "bitnsbot/txwatches"
import "bitnsbot/watches"

type watchType string

const watchTypeTransaction watchType = "transaction"
const watchTypeAddress watchType = "address"

type notification struct {
    txid         string
    received     map[string]float64
    fee          float64 // BTC
    feeRate      float64 // sat/vB
    confEstimate string  // "~10-20 min" etc; "" if unavailable
    feeOK        bool    // whether fee/feeRate are populated
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
                    if amount, ok := n.received[watchID]; ok {
                        var pairs = [][2]string{
                            {"Tx", short(n.txid)},
                            {"Amount", amountLine(amount, time.Time{}, true)},
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
                        send(b, chatID, "🔔 New transaction on watched address "+label+"\n\n<pre>"+strings.Join(lines, "\n")+"</pre>")
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

// notifier handles btcd's push notifications. Rather than wrapping it in
// jsonrpc2.AsyncHandler, Handle spawns the work itself: broadcast, cacheBlockHash
// and checkConfirmations all call back into btcd, which would deadlock the
// connection's single read loop if run inside a synchronous Handle. It holds the
// bot so checkConfirmations can message chats about their confirmed transactions.
type notifier struct{ bot *bot }

func (n notifier) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
    if req.Params == nil { return }
    switch req.Method {
    case "relevanttxaccepted":
        var params []string
        if err := json.Unmarshal(*req.Params, &params); err == nil && len(params) > 0 {
            go broadcast(params[0])
        }
    case "blockconnected":
        var params []json.RawMessage
        if err := json.Unmarshal(*req.Params, &params); err == nil && len(params) > 0 {
            var hash string
            if json.Unmarshal(params[0], &hash) == nil {
                go cacheBlockHash(hash)
                go checkConfirmations(n.bot, hash)
            }
        }
    }
}

func broadcast(txHex string) {
    if btcd == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var tx, err = btcd.decodeRawTransaction(ctx, txHex)
    if err != nil {
        logging.Err("decode notified tx: %v", err)
        return
    }
    var n = notification{txid: tx.Txid, received: make(map[string]float64)}
    for _, vout := range tx.Vout {
        var addrs = vout.ScriptPubKey.Addresses
        if a := vout.ScriptPubKey.Address; a != "" && !slices.Contains(addrs, a) {
            addrs = append(addrs, a)
        }
        for _, addr := range addrs {
            n.received[addr] += vout.Value
        }
    }
    if full, ferr := btcd.getRawTransaction(ctx, tx.Txid); ferr == nil && full.Vsize > 0 {
        if fee, _, ok := txInputs(ctx, full); ok {
            n.fee = fee
            n.feeRate = math.Round(fee*1e8) / float64(full.Vsize)
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

// confEstimate maps a transaction's fee rate (sat/vB) to a rough confirmation
// window by comparing it to btcd's fee estimates (BTC/kB → sat/vB via ×1e5) for
// the 2- and 6-block targets. Returns "" if the estimator has no data yet.
func confEstimate(ctx context.Context, feeRate float64) string {
    var fast, ferr = btcd.estimateFee(ctx, 2)
    var medium, merr = btcd.estimateFee(ctx, 6)
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
// on startup and, if btcd is connected, loads the watched addresses into btcd's
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
    if btcd != nil && len(addrs) > 0 {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        if err := btcd.loadTxFilter(ctx, true, addrs, nil); err != nil {
            logging.Warn("load tx filter: %v", err)
        }
        cancel()
    }
}

// reapplyBtcdState re-establishes btcd's stateful subscriptions after a
// reconnect — the block-notification subscription and the transaction filter for
// every watched address. Passed to btcd.supervise as its reconnect callback.
func reapplyBtcdState() {
    if btcd == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if err := btcd.notifyBlocks(ctx); err != nil {
        logging.Warn("resubscribe to blocks: %v", err)
    }
    var addrs = notifyAddresses()
    if len(addrs) > 0 {
        if err := btcd.loadTxFilter(ctx, true, addrs, nil); err != nil {
            logging.Warn("reload tx filter: %v", err)
        }
    }
}

func stopNotify() {
    notifyMu.Lock()
    for _, v := range notifies {
        close(v.stop)
    }
    notifies = make(map[notifyKey]notifyChans)
    notifyMu.Unlock()
    txwatches.Reset()
}

// checkConfirmations notifies and drops every transaction watch whose transaction
// appears in the just-connected block. The block's txids come from a light
// getblock (verbosity 1); the fetch is skipped entirely when nothing is watched,
// so an idle bot pays nothing per block. Runs off btcd's read-loop goroutine
// (spawned by notifier.Handle) since it calls back into btcd.
func checkConfirmations(b *bot, hash string) {
    if b == nil || btcd == nil { return }
    if !txwatches.Any() { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    var blk, err = btcd.getBlockTxids(ctx, hash)
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
        send(b, c.ChatID, msg)
        logging.Info("confirmed watch %s for chat %d in block %d", short(c.Txid), c.ChatID, blk.Height)
    }
}
