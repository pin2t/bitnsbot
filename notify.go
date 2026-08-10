package main

import "context"
import "fmt"
import "html"
import "math"
import "strconv"
import "slices"
import "sort"
import "strings"
import "sync"
import "time"

import "bitnsbot/logging"
import "bitnsbot/txwatches"
import "bitnsbot/watches"

type notification struct {
    txid         string
    received     map[string]float64 // address → value paid to it by this tx's outputs
    sent         map[string]float64 // address → value spent from it by this tx's inputs
    fee          float64            // BTC
    feeRate      float64            // sat/vB
    confEstimate string             // confETAXXX, "" if unavailable
    feeOK        bool               // whether fee/feeRate are populated
}

type notifyKey struct {
    chat int64
    id   string
}

type notifyChans struct {
    ch   chan notification
    stop chan struct{}
}

var notifyMu sync.Mutex
var notifies = make(map[notifyKey]notifyChans)

func startNotifyChat(b *bot, chat int64, watch, alias string) {
    var ch = make(chan notification)
    var stop = make(chan struct{})
    notifyMu.Lock()
    notifies[notifyKey{chat, watch}] = notifyChans{ch, stop}
    notifyMu.Unlock()
    go func(b *bot, chat int64, watch, alias string, ch <-chan notification, stop chan struct{}) {
        for {
            select {
            case <-stop:
                return
            case n := <-ch:
                var msg, ids, summary, ok = addressNotification(chat, n, watch, alias)
                if !ok { continue }
                send(b, chat, msg, ids)
                txwatches.AddAddrConfirm(n.txid, chat, watch, alias, summary)
            }
        }
    }(b, chat, watch, alias, ch, stop)
}

// addressNotification renders the mempool message for a transaction touching a
// watched address, the ids its buttons carry, and the summary the later
// confirmation message restates. ok is false when the transaction turns out not
// to involve this address at all (every watcher sees every broadcast).
//
// The headline is a sentence rather than a field, so the whole thing reads at a
// glance in a notification preview: the amount, where it is going, and the full
// txid in <code> for tap-to-copy. Only the supporting figures stay in the <pre>
// block. The txid has to sit outside that block — Telegram does not parse
// entities inside <pre>, so a <code> there would render literally.
func addressNotification(chat int64, n notification, watchID, alias string) (string, []string, txwatches.Summary, bool) {
    var estimates = map[string]string{
        confETAFast:   i18n(chat).String("~10-20 min"),
        confETAMedium: i18n(chat).String("~1 hour"),
        confETASlow:   i18n(chat).String("2+ hours"),
    }
    var in, gotIn = n.received[watchID]
    var out, gotOut = n.sent[watchID]
    if !gotIn && !gotOut { return "", nil, txwatches.Summary{}, false }
    var label = short(watchID)
    if alias != "" { label += " (" + html.EscapeString(alias) + ")" }
    var ids = []string{n.txid, watchID}
    var pairs [][2]string
    var header string
    var summary txwatches.Summary
    if gotOut {
        type pair struct {a string; b int}
        var recipients []pair
        var total int
        var addrs = make([]string, 0)
        for addr, value := range n.received {
            if addr == watchID { continue }
            recipients = append(recipients, pair{addr, int(value * 1e8)})
            addrs = append(addrs, addr)
            total += int(value * 1e8)
        }
        sort.Slice(recipients, func(i, j int) bool {
            if n.received[recipients[i].a] != n.received[recipients[j].a] {
                return n.received[recipients[i].a] > n.received[recipients[j].a]
            }
            return recipients[i].b < recipients[j].b
        })
        summary = txwatches.Summary{Amount: out, Recipients: addrs, Outgoing: true}
        header = i18n(chat).Sprintf("%s is sending %s to\n", label, amountLine(out, time.Time{}, true, chat))
        if len(recipients) == 0 { header += i18n(chat).String("none") + "\n" }
        for i := 0; i < min(len(recipients), shownAddrs); i++ {
            header += fmt.Sprintf("%s: %s\n", short(recipients[i].a), amountLine(float64(recipients[i].b)/1e8, time.Now(), true, chat))
            ids = append(ids, recipients[i].a)
        }
        if len(recipients) > shownAddrs { header += "...\n" }
        pairs = append(pairs, [2]string{i18n(chat).String("Sending"), amountLine(out, time.Time{}, true, chat)})
        if gotIn {
            // part of the spend came back as change, so the address is only down
            // by the difference
            pairs = append(pairs,
                [2]string{i18n(chat).String("Change back"), amountLine(in, time.Time{}, true, chat)},
                [2]string{i18n(chat).String("Net"), amountLine(in-out, time.Time{}, true, chat)},
            )
        }
        if n.feeOK {
            pairs = append(pairs, [2]string{
                i18n(chat).String("Fee"),
                sats(n.fee) + " " + i18n(chat).Sprintf("sats") + " (" + strings.TrimSuffix(strconv.FormatFloat(n.feeRate, 'f', 1, 64), ".0") + " " + i18n(chat).String("sat/vB") + ")"})
            if n.confEstimate != "" {
                pairs = append(pairs, [2]string{i18n(chat).String("ETA"), estimates[n.confEstimate]})
            }
        }
    } else {
        var msg = i18n(chat).Sprintf("🔔 %s receiving %s. Transaction <code>%s</code>", label, amountLine(in, time.Time{}, true, chat), n.txid)
        if n.confEstimate != "" {
            msg += "\n" + i18n(chat).String("ETA") + " " + estimates[n.confEstimate]
        }
        return msg, ids, txwatches.Summary{Amount: in}, true
    }
    header += i18n(chat).Sprintf("Transaction <code>%s</code>", n.txid)
    return fmt.Sprintf("🔔 %s%s", header, fields(pairs)), ids, summary, true
}

// fields renders the aligned <pre> block a message hangs beneath, or nothing at
// all when there is nothing to show — a coinbase or a transaction first seen
// already mined has no fee or confirmation estimate to report.
func fields(pairs [][2]string) string {
    if len(pairs) == 0 { return "" }
    return "\n\n<pre>" + joinAlign(pairs) + "</pre>"
}

func stopNotifyChat(chat int64, id string) {
    notifyMu.Lock()
    defer notifyMu.Unlock()
    var key = notifyKey{chat, id}
    if c, found := notifies[key]; found {
        close(c.stop)
        delete(notifies, key)
    }
}

func notifyAddresses() (res []string) {
    notifyMu.Lock()
    defer notifyMu.Unlock()
    res = make([]string, 0, len(notifies))
    for k := range notifies {
        if !slices.Contains(res, k.id) {
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
            n.confEstimate = confEstimate(n.feeRate)
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
// outpoint it consumes, never the address, so recognising one means already
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

const confETAFast = "fast"
const confETAMedium = "med"
const confETASlow = "slow"

// confEstimate maps a transaction's fee rate (sat/vB) to a rough confirmation
// window by comparing it to the cached mempool-based fee recommendations. Returns
// "" until the first background recomputation completes.
func confEstimate(feeRate float64) string {
    feesMu.Lock()
    var rec, ok = cachedFees, cachedFeesOK
    feesMu.Unlock()
    if !ok {
        return ""
    }
    switch {
    case feeRate >= rec.fastest: return confETAFast
    case feeRate >= rec.hour:    return confETAMedium
    default:                     return confETASlow
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
        startNotifyChat(bot, w.Chat, w.Address, w.Alias)
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

// confirmationMessage restates what the mempool notification said and adds where
// and when it landed. It carries no <pre> block: everything it reports is one
// sentence, and the txid needs to be outside such a block anyway to be
// tap-to-copy. The amount and recipients come from the summary recorded when the
// transaction was first seen, so the block arriving costs no extra lookups.
func confirmationMessage(chat int64, c txwatches.Confirmed, height int64) (string, []string) {
    var elapsed = durationText(time.Since(c.WatchedAt), chat)
    var landed = i18n(chat).Sprintf("Confirmed in block #%d after %s. Transaction <code>%s</code>", height, elapsed, c.Txid)
    var ids = []string{c.Txid}
    if c.Addr == "" {
        var label = short(c.Txid)
        if c.Alias != "" {
            label += " (" + html.EscapeString(c.Alias) + ")"
        }
        ids = append(ids, strconv.FormatInt(height, 10))
        return i18n(chat).Sprintf("🔔 Transaction %s was confirmed in block #%d after %s", label, height, elapsed), ids
    }
    var label = short(c.Addr)
    if c.Alias != "" {
        label += " (" + html.EscapeString(c.Alias) + ")"
    }
    ids = append(ids, c.Addr)
    var msg string
    if c.Summary.Outgoing {
        var saddr string
        if len(c.Summary.Recipients) > 0 {
            saddr = compactAddrs(c.Summary.Recipients)
        } else {
            saddr = i18n(chat).String("none")
        }
        msg = i18n(chat).Sprintf("%s sent %s to %s. %s", label, amountLine(c.Summary.Amount, time.Time{}, true, chat), saddr, landed)
        ids = append(ids, firstN(c.Summary.Recipients, shownAddrs)...)
    } else {
        msg = i18n(chat).Sprintf("%s received %s. %s", label, amountLine(c.Summary.Amount, time.Time{}, true, chat), landed)
    }
    ids = append(ids, strconv.FormatInt(height, 10))
    return "🔔 " + msg, ids
}

// processConfirms notifies and drops every transaction watch whose transaction
// appears in the just-connected block. The block's txids come from a light
// getblock (verbosity 1); the fetch is skipped entirely when nothing is watched,
// so an idle bot pays nothing per block. Runs off core's read-loop goroutine
// (spawned by notifier.Handle) since it calls back into core.
func processConfirms(b *bot, hash string) {
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
        var msg, ids = confirmationMessage(c.ChatID, c, blk.Height)
        send(b, c.ChatID, msg, ids)
        logging.Info("confirmed watch %s for chat %d in block %d", short(c.Txid), c.ChatID, blk.Height)
    }
}
