package main

import "bitnsbot/app"
import "context"
import "html"
import "encoding/hex"
import "errors"
import "sort"
import "strconv"
import "strings"
import "sync"
import "time"
import "bitnsbot/addrindex"
import "bitnsbot/addrstat"
import "bitnsbot/core"
import "bitnsbot/cursors"
import "bitnsbot/logging"
import "bitnsbot/rates"

var pendingInfoMu sync.Mutex
var pendingInfoChats = make(map[int64]bool)

func info(bot *bot, chat int64, arg string) {
    if arg == "" {
        pendingInfoMu.Lock()
        pendingInfoChats[chat] = true
        pendingInfoMu.Unlock()
        send(bot, chat, i18n(chat).String("Please send Bitcoin address or transaction or block number or block hash"), nil)
        return
    }
    pendingInfoMu.Lock()
    delete(pendingInfoChats, chat)
    pendingInfoMu.Unlock()
    if !core.Enabled() {
        send(bot, chat, i18n(chat).String("Bitcoin node connection is not configured"), nil)
        return
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if len(arg) == 64 {
        if app.IsTxID(arg) {
            transaction(ctx, bot, chat, arg)
            return
        }
        if header, err := core.GetBlockHeader(ctx, arg); err == nil {
            block(ctx, bot, chat, header.Height)
            return
        }
    }
    if height, err := strconv.ParseInt(arg, 10, 64); err == nil && height >= 0 {
        block(ctx, bot, chat, height)
        return
    }
    address(ctx, bot, chat, arg)
}

// txData is everything one transaction is described by: the lines /info
// prints, the ids those lines turn into buttons, the node's own spelling of
// the txid, whether it is confirmed, and the two sides of the app's
// input/output flow. txPairs fills it; the bot's reply and the Mini App's
// transaction page read the same values, so the two cannot drift apart.
type txData struct {
    pairs     [][2]string
    ids       []string
    canonical string
    confirmed bool
    inputs    []app.FlowPart
    outputs   []app.FlowPart
}

// txPairs builds the lines a transaction is described by, plus the ids the bot
// turns into buttons, the node's own spelling of the txid, whether the
// transaction is confirmed, and the app's input/output flow parts.
//
// only the ids the text actually shows get buttons — compactAddrs truncates
// to shownAddrs with a trailing "...", and a button for something the reader
// cannot see in the message would be a puzzle rather than a shortcut
func txPairs(ctx context.Context, lang string, txid string) (txData, bool) {
    var out txData
    var estimates = map[string]string{
        confETAFast:   i18nl(lang).String("~10-20 min"),
        confETAMedium: i18nl(lang).String("~1 hour"),
        confETASlow:   i18nl(lang).String("2+ hours"),
    }
    var tx, err = core.GetRawTransaction(ctx, txid)
    if err != nil { return out, false }
    var total int64
    for _, vout := range tx.Vout { total += toSat(vout.Value) }
    var coinbase = len(tx.Vin) > 0 && tx.Vin[0].Coinbase != ""
    var fee int64
    var inputs []string
    var inSats []int64
    var feeOK bool
    if !coinbase { fee, inputs, inSats, _, feeOK = txInputs(ctx, tx) }
    var outputs = outputAddrs(tx)
    var at = time.Time{}
    var current = true
    var blockHeight int64
    if tx.Confirmations == 0 {
        var confText = i18nl(lang).String("none (confirms in ~10-20 min)")
        if feeOK && tx.Vsize > 0 {
            confText = i18nl(lang).String("none (confirms in") + " " + estimates[confEstimate(float64(fee) / float64(tx.Vsize))] + ")"
        }
        out.pairs = append(out.pairs, [2]string{i18nl(lang).String("Confirmations"), confText})
    } else {
        at, current = time.Unix(tx.Time, 0), false
        if header, err := core.GetBlockHeader(ctx, tx.BlockHash); err == nil {
            blockHeight = header.Height
        }
        out.pairs = append(out.pairs, [2]string{i18nl(lang).String("Confirmations"), i18nl(lang).Sprintf("%d (block #%d)", tx.Confirmations, blockHeight)})
    }
    var rate, rateOK = usdRate(at, current)
    out.pairs = append(out.pairs, [2]string{i18nl(lang).String("Amount"), amountLine(total, at, current, lang)})
    if feeOK {
        var feeStr = group(fee) + " " + i18nl(lang).String("sats")
        if tx.Vsize > 0 {
            var feeRate = float64(fee) / float64(tx.Vsize)
            feeStr += " (" + trimNum(feeRate, 1) + i18nl(lang).String(" sat/vB") + ")"
        }
        out.pairs = append(out.pairs, [2]string{i18nl(lang).String("Fee"), feeStr})
    }
    var szMsg = group(int64(tx.Size)) + " " + i18nl(lang).String("B")
    if tx.Vsize > 0 { szMsg += i18nl(lang).Sprintf(" (%s vB)", group(int64(tx.Vsize))) }
    out.pairs = append(out.pairs, [2]string{i18nl(lang).String("Size"), szMsg})
    if feeOK {
        out.pairs = append(out.pairs, [2]string{i18nl(lang).String("Inputs"), compactAddrs(inputs)})
    }
    out.pairs = append(out.pairs, [2]string{i18nl(lang).String("Outputs"), compactAddrs(outputs)})
    if blockHeight > 0 {
        out.ids = append(out.ids, strconv.FormatInt(blockHeight, 10))
    }
    if tx.BlockHash != "" { out.ids = append(out.ids, tx.BlockHash) }
    out.ids = append(out.ids, firstN(inputs, shownAddrs)...)
    out.ids = append(out.ids, firstN(outputs, shownAddrs)...)
    for i, a := range inputs {
        var p = addrPart(a)
        p.Amount = flowAmount(inSats[i], rate, rateOK, lang)
        out.inputs = append(out.inputs, p)
    }
    for _, v := range tx.Vout {
        var p = addrPart(addressOf(v))
        p.Amount = flowAmount(toSat(v.Value), rate, rateOK, lang)
        out.outputs = append(out.outputs, p)
    }
    out.canonical, out.confirmed = tx.Txid, tx.Confirmations > 0
    return out, true
}

func transaction(ctx context.Context, bot *bot, chat int64, txid string) {
    var d, ok = txPairs(ctx, chatLang(chat), txid)
    if !ok {
        send(bot, chat, i18n(chat).Sprintf("Couldn't find transaction %s", short(txid)), nil)
        return
    }
    send(bot, chat, i18n(chat).Sprintf("Transaction <code>%s</code>\n\n<pre>%s</pre>", d.canonical, joinAlign(d.pairs)), d.ids)
}

// txInputs reports a transaction's fee, the addresses it spends from and the
// amount of each input — addresses and amounts in input order (the addresses for
// the /info listing and the app's flow), and the amounts also summed per address
// as spent (which is how the watch notifier learns an address is *sending*, the
// counterpart to the receiving addresses it reads straight off the outputs).
//
// Core hands the prevouts over inline at getrawtransaction verbosity 2, so a
// confirmed transaction needs no extra calls at all — the bounded prevout
// fan-out btcd forced on us is gone. That only holds for confirmed transactions
// though: a mempool transaction has no undo data, so Core omits both fee and
// prevout there and each input's previous transaction still has to be fetched.
// A fetch failure yields ok=false, so the reply degrades to "unavailable"
// rather than showing a wrong fee.
func txInputs(ctx context.Context, tx *core.Transaction) (fee int64, addrs []string, sats []int64, spent map[string]int64, ok bool) {
    spent = make(map[string]int64)
    var inSum int64
    var complete = true
    for _, vin := range tx.Vin {
        if vin.PrevOut == nil { complete = false; break }
        var v = toSat(vin.PrevOut.Value)
        inSum += v
        var a = addressOfScript(vin.PrevOut.ScriptPubKey)
        addrs = append(addrs, a)
        sats = append(sats, v)
        spent[a] += v
    }
    if complete {
        if tx.Fee > 0 { return toSat(tx.Fee), addrs, sats, spent, true }
        return inputsMinusOutputs(inSum, tx), addrs, sats, spent, true
    }
    return fetchInputs(ctx, tx)
}

// fetchInputs is the mempool path: without undo data Core cannot supply prevouts,
// so they are fetched concurrently (bounded, the same pattern the btcd client
// used) and the fee derived from inputs − outputs.
func fetchInputs(ctx context.Context, tx *core.Transaction) (fee int64, addrs []string, sats []int64, spent map[string]int64, ok bool) {
    var ids = map[string]bool{}
    for _, in := range tx.Vin {
        ids[in.Txid] = true
    }
    var prevouts = map[string]*core.Transaction{}
    var mu sync.Mutex
    var wg sync.WaitGroup
    var sem = make(chan struct{}, 16)
    var fetchErr error
    for id := range ids {
        wg.Add(1)
        sem <- struct{}{}
        go func(id string) {
            defer wg.Done()
            defer func() { <-sem }()
            var p, e = core.GetRawTransaction(ctx, id)
            mu.Lock()
            if e != nil {
                if fetchErr == nil { fetchErr = e }
            } else {
                prevouts[id] = p
            }
            mu.Unlock()
        }(id)
    }
    wg.Wait()
    if fetchErr != nil {
        return 0, nil, nil, nil, false
    }
    var inSum int64
    spent = make(map[string]int64)
    for _, vin := range tx.Vin {
        var p = prevouts[vin.Txid]
        if p == nil || int(vin.Vout) >= len(p.Vout) {
            return 0, nil, nil, nil, false
        }
        var v = toSat(p.Vout[vin.Vout].Value)
        inSum += v
        var a = addressOf(p.Vout[vin.Vout])
        addrs = append(addrs, a)
        sats = append(sats, v)
        spent[a] += v
    }
    return inputsMinusOutputs(inSum, tx), addrs, sats, spent, true
}

func inputsMinusOutputs(inSum int64, tx *core.Transaction) int64 {
    var outSum int64
    for _, v := range tx.Vout {
        outSum += toSat(v.Value)
    }
    if fee := inSum - outSum; fee > 0 { return fee }
    return 0
}

func firstN(s []string, n int) []string {
    if len(s) > n { return s[:n] }
    return s
}

func outputAddrs(tx *core.Transaction) []string {
    var addrs []string
    for _, v := range tx.Vout {
        addrs = append(addrs, addressOf(v))
    }
    return addrs
}

func addressOf(v core.Vout) string { return addressOfScript(v.ScriptPubKey) }

// addressOfScript names the address an output pays. Core reports a single
// "address" field; the plural "addresses" array btcd used (and old Core versions
// emitted for bare multisig) is gone, so there is only the one field to read.
func addressOfScript(s core.ScriptPubKey) string {
    if s.Address != "" { return s.Address }
    return "(non-standard)"
}

// compactAddrs joins shortened addresses, showing at most the first three with a
// trailing "..." when there are more.
// shownAddrs is how many of a transaction's input/output addresses a reply
// lists. The buttons use it too, so every id with a button is an id the reader
// can actually see in the text.
const shownAddrs = 3

func compactAddrs(addrs []string) string {
    if len(addrs) == 0 { return "none" }
    var show, more = addrs, false
    if len(addrs) > shownAddrs {
        show, more = addrs[:shownAddrs], true
    }
    var parts []string
    for _, a := range show {
        parts = append(parts, short(a))
    }
    var s = strings.Join(parts, ", ")
    if more { s += ", ..." }
    return s
}

func block(ctx context.Context, bot *bot, chat int64, height int64) {
    if bi, ok := loadBlock(height); ok {
        send(bot, chat, formatBlock(bi, chatLang(chat)), nil)
        return
    }
    var hash, err = core.GetBlockHash(ctx, height)
    if err != nil {
        send(bot, chat, i18n(chat).Sprintf("Couldn't find block %d", height), nil)
        return
    }
    var bi, ciErr = computeBlockInfo(ctx, hash)
    if ciErr != nil {
        logging.Err("compute block %d: %v", height, ciErr)
        send(bot, chat, i18n(chat).String("Sorry, something went wrong fetching that block"), nil)
        return
    }
    send(bot, chat, formatBlock(bi, chatLang(chat)), nil)
}

// feeStats summarises a block's fee distribution. Core reports each
// transaction's fee directly in getblock verbosity 2, so unlike the btcd path
// this needs no prevout fetching at all — the bounded 16-way fan-out that used
// to be here existed only because btcd made callers compute fees themselves.
// The coinbase has no fee and is skipped.
//
// coinbase
func feeStats(txs []core.Transaction) (low, avg, high int64, count int) {
    var total int64
    for i, t := range txs {
        if i == 0 { continue }
        var fee = toSat(t.Fee)
        if count == 0 || fee < low { low = fee }
        if fee > high { high = fee }
        total += fee
        count++
    }
    if count == 0 { return 0, 0, 0, 0 }
    return low, (total + int64(count)/2) / int64(count), high, count
}

// addrTxLimit bounds how many of an address's transactions are resolved for the
// stats, the same cap (and the same trailing "+") the btcd path applied to
// searchrawtransactions. The index itself stores the full history; this only
// bounds the work one /info reply does.
var addrTxLimit = 10000

// addressHistory turns an address's index touches into resolved transactions.
// The index stores (height, txIndex) rather than txids — which is what lets it
// spend 10 bytes per touch instead of 32 — so resolving means reading each
// block's txid list once (getblock verbosity 1) and then the transactions
// themselves at verbosity 2, where Core supplies the prevouts and fee inline.
// Both stages are concurrent and bounded, the same pattern the rest of the bot
// uses. complete is false when the cap or the caller's deadline cut it short.
func addressHistory(ctx context.Context, script []byte) (txs []*core.Transaction, complete bool) {
    var touches, capped = addrindex.Lookup(script, 10000)
    if len(touches) == 0 { return nil, !capped }
    if len(touches) > addrTxLimit {
        touches, capped = touches[:addrTxLimit], true
    }
    var byHeight = map[uint32][]uint16{}
    var heights []uint32
    for _, t := range touches {
        if _, seen := byHeight[t.Height]; !seen { heights = append(heights, t.Height) }
        byHeight[t.Height] = append(byHeight[t.Height], t.TxIndex)
    }
    var mu sync.Mutex
    var ids []string
    var wg sync.WaitGroup
    var sem = make(chan struct{}, 16)
    var failed bool
    for _, h := range heights {
        wg.Add(1)
        sem <- struct{}{}
        go func(h uint32) {
            defer wg.Done()
            defer func() { <-sem }()
            var hash, err = core.GetBlockHash(ctx, int64(h))
            if err != nil { mu.Lock(); failed = true; mu.Unlock(); return }
            var blk, berr = core.GetBlockTxids(ctx, hash)
            if berr != nil { mu.Lock(); failed = true; mu.Unlock(); return }
            mu.Lock()
            for _, idx := range byHeight[h] {
                if int(idx) < len(blk.Tx) { ids = append(ids, blk.Tx[idx]) }
            }
            mu.Unlock()
        }(h)
    }
    wg.Wait()
    for _, id := range ids {
        wg.Add(1)
        sem <- struct{}{}
        go func(id string) {
            defer wg.Done()
            defer func() { <-sem }()
            var tx, err = core.GetRawTransaction(ctx, id)
            if err != nil { mu.Lock(); failed = true; mu.Unlock(); return }
            mu.Lock()
            txs = append(txs, tx)
            mu.Unlock()
        }(id)
    }
    wg.Wait()
    sort.Slice(txs, func(i, j int) bool { return txs[i].Time < txs[j].Time })
    return txs, !capped && !failed && ctx.Err() == nil
}

// addrTxBatch is how many transaction views one batch holds: the first batch is
// part of the address page, and each scroll appends another.
const addrTxBatch = 5

// addressTxViews resolves one batch of an address's transactions for the app's
// transaction views, newest first. from is how many views the reader has
// already been shown, so the batch is the addrTxBatch transactions before that
// point in history. The scan starts at the block the address last appeared in
// — its statistics record's last-activity time, turned into a height through
// the blocks table — rather than at the tip, and reads the index backwards from
// there. One touch more than the batch is fetched as a sentinel: if it is
// there, there is a page after this one. ok is false when the address is not
// valid or the lookup failed — a different answer from a valid address with no
// transactions, which is an empty batch.
func addressTxViews(ctx context.Context, lang, addr string, from int) (views []app.Tx, more, ok bool) {
    var ai, err = core.ValidateAddress(ctx, addr)
    if err != nil || !ai.IsValid { return nil, false, false }
    var script, derr = hex.DecodeString(ai.ScriptPubKey)
    if derr != nil { return nil, false, false }
    var touches, _ = addrindex.LookupFrom(script, txStartHeight(addr), from+addrTxBatch+1)
    if len(touches) == 0 { return nil, false, true }
    var window []addrindex.Touch
    if len(touches) > from+addrTxBatch {
        more = true
        window = touches[from : from+addrTxBatch]
    } else {
        if from >= len(touches) { return nil, false, true }
        window = touches[from:]
    }
    for _, tx := range resolveTouches(ctx, window) {
        views = append(views, txView(tx, addr, lang))
    }
    return views, more, true
}

// txStartHeight is where the address index scan for an address begins: the
// block holding its last activity, read from its statistics record. A record
// is only trusted when the statistics scan has kept up with the index —
// otherwise the newest indexed touches are not in it yet, and the scan starts
// from the tip instead.
func txStartHeight(addr string) uint32 {
    var s, ok = addrstat.Get(addr)
    if !ok || s.Last <= 0 { return 0 }
    var statCursor, statOK = cursors.Get(cursors.AddrStat)
    var indexCursor, indexOK = addrindex.Cursor()
    if !statOK || !indexOK || statCursor < int64(indexCursor) { return 0 }
    if h, ok := blockByTime(s.Last); ok && h > 0 { return uint32(h) }
    return 0
}

// resolveTouches turns index touches into their transactions, newest first. The
// touches are already a small newest-first window, so only those blocks are
// read — getblock for each height, then getrawtransaction for each txid — the
// same bounded fan-out the rest of the bot uses.
func resolveTouches(ctx context.Context, touches []addrindex.Touch) []*core.Transaction {
    if len(touches) == 0 { return nil }
    var byHeight = map[uint32][]uint16{}
    var heights []uint32
    for _, t := range touches {
        if _, seen := byHeight[t.Height]; !seen { heights = append(heights, t.Height) }
        byHeight[t.Height] = append(byHeight[t.Height], t.TxIndex)
    }
    var mu sync.Mutex
    var ids = map[uint32]map[uint16]string{}
    var order []addrindex.Touch
    var wg sync.WaitGroup
    var sem = make(chan struct{}, 16)
    for _, h := range heights {
        wg.Add(1)
        sem <- struct{}{}
        go func(h uint32) {
            defer wg.Done()
            defer func() { <-sem }()
            var hash, err = core.GetBlockHash(ctx, int64(h))
            if err != nil { return }
            var blk, berr = core.GetBlockTxids(ctx, hash)
            if berr != nil { return }
            mu.Lock()
            for _, idx := range byHeight[h] {
                if int(idx) < len(blk.Tx) {
                    if ids[h] == nil { ids[h] = map[uint16]string{} }
                    ids[h][idx] = blk.Tx[idx]
                }
            }
            mu.Unlock()
        }(h)
    }
    wg.Wait()
    for _, t := range touches {
        if ids[t.Height][t.TxIndex] != "" { order = append(order, t) }
    }
    if len(order) == 0 { return nil }
    var found = map[string]*core.Transaction{}
    for _, t := range order {
        var id = ids[t.Height][t.TxIndex]
        wg.Add(1)
        sem <- struct{}{}
        go func(id string) {
            defer wg.Done()
            defer func() { <-sem }()
            var tx, err = core.GetRawTransaction(ctx, id)
            if err != nil { return }
            mu.Lock()
            found[id] = tx
            mu.Unlock()
        }(id)
    }
    wg.Wait()
    var out []*core.Transaction
    for _, t := range order {
        if tx := found[ids[t.Height][t.TxIndex]]; tx != nil { out = append(out, tx) }
    }
    return out
}

// txView builds one transaction view for an address page: when it happened,
// what the address gained or lost in it, and the addresses on both sides. The
// amount is the net move for this address — the sum of the outputs paying it
// minus the sum of the inputs it spent — signed so a glance shows the
// direction, and it switches from sats to BTC at the same threshold the rest
// of the app uses — 0.05 BTC — with the dollar value under it the same
// historical approximation amountLine prints, at the rate nearest the
// transaction's own time.
func txView(tx *core.Transaction, addr, lang string) app.Tx {
    var delta int64
    for _, v := range tx.Vout {
        if v.ScriptPubKey.Address == addr { delta += toSat(v.Value) }
    }
    for _, in := range tx.Vin {
        if in.PrevOut != nil && in.PrevOut.ScriptPubKey.Address == addr {
            delta -= toSat(in.PrevOut.Value)
        }
    }
    var v = app.Tx{Id: tx.Txid, Short: short(tx.Txid), Time: day(tx.Time, lang), Amount: signedAmountText(delta, lang)}
    var rate float64
    var rateOK bool
    if tx.Time > 0 { rate, rateOK = rates.At(time.Unix(tx.Time, 0)) }
    if !rateOK { rate, rateOK = rates.Last() }
    if rateOK { v.USD = "≈ " + usd(delta, rate) }
    if len(tx.Vin) == 0 || tx.Vin[0].Coinbase == "" {
        for _, in := range tx.Vin {
            v.Inputs = append(v.Inputs, inputPart(in))
        }
    }
    for _, out := range tx.Vout {
        v.Outputs = append(v.Outputs, addrPart(out.ScriptPubKey.Address))
    }
    return v
}

// inputPart names one input's address the same way the rest of the bot does; a
// coinbase has none, and a missing prevout reads as the non-standard
// placeholder rather than as an address nothing can open.
func inputPart(in core.Vin) app.FlowPart {
    if in.PrevOut == nil { return app.FlowPart{Text: "(non-standard)"} }
    return addrPart(in.PrevOut.ScriptPubKey.Address)
}

// addrPart is one address on either side of a transaction flow: clickable when
// it is an address, plain text when it is not.
func addrPart(a string) app.FlowPart {
    if a == "" || a == "(non-standard)" { return app.FlowPart{Text: "(non-standard)"} }
    return app.FlowPart{Text: short(a), Id: a}
}

// usdRate is the rate a transaction flow's USD estimates use: the rate nearest
// a confirmed transaction's time, or the latest stored rate for a mempool one,
// which has no confirmed time to look a historical rate up at.
func usdRate(at time.Time, current bool) (float64, bool) {
    if current { return rates.Last() }
    var rate, ok = rates.At(at)
    if !ok { return rates.Last() }
    return rate, true
}

// flowAmount formats the amount one side of a transaction flow moved: amountText's
// sats/BTC figure with the dollar value in-line when a rate is known, the way
// amountLine prints the bot's own Amount row. The rate is computed once per
// transaction, so every address of one flow reads the same price.
func flowAmount(sat int64, rate float64, rateOK bool, lang string) string {
    if !rateOK { return amountText(sat, lang) }
    return amountText(sat, lang) + " (≈ " + usd(sat, rate) + ")"
}

// addressStats sums an address's on-chain history from its transactions: total
// received (outputs paying it), total sent (inputs spending from it), fees on its
// outgoing transactions, and the earliest/latest confirmed transaction times.
// Confirmed transactions carry their prevouts and fee from Core directly, so both
// the sending side and the fee read straight off the transaction.
func addressStats(txs []*core.Transaction, addr string) (received, sent, fees int64, firstT, lastT int64) {
    for _, tx := range txs {
        for _, v := range tx.Vout {
            if v.ScriptPubKey.Address == addr { received += toSat(v.Value) }
        }
        var fromAddr bool
        for _, in := range tx.Vin {
            if in.PrevOut != nil && in.PrevOut.ScriptPubKey.Address == addr {
                sent += toSat(in.PrevOut.Value)
                fromAddr = true
            }
        }
        if fromAddr { fees += toSat(tx.Fee) }
        if tx.Time > 0 {
            if firstT == 0 || tx.Time < firstT { firstT = tx.Time }
            if tx.Time > lastT { lastT = tx.Time }
        }
    }
    return
}

// statPairs renders a stored record into the same lines, in the same order, that
// the live path builds — the two answers must not be told apart by their shape,
// only by how long they took. The transaction count carries no trailing "+":
// this history is whole, which is the point of gathering it.
func statPairs(addr string, s addrstat.Stat, lang string) [][2]string {
    var pairs = [][2]string{
        {i18nl(lang).String("Type"), addrTypeText(s.Type, addr)},
        {i18nl(lang).String("Balance"), compactBTC(s.Balance, lang)},
        {i18nl(lang).String("Total received"), compactBTC(s.Recv, lang)},
        {i18nl(lang).String("Total sent"), compactBTC(s.Sent, lang)},
        {i18nl(lang).String("Total flow"), compactBTC(s.Flow, lang)},
        {i18nl(lang).String("Total fees"), compactBTC(s.Fees, lang)},
        {i18nl(lang).String("Transactions"), group(s.Txs)},
    }
    if s.First > 0 { pairs = append(pairs, [2]string{i18nl(lang).String("First tx"), day(s.First, lang)}) }
    if s.Last > 0 { pairs = append(pairs, [2]string{i18nl(lang).String("Last tx"), day(s.Last, lang)}) }
    if s.First > 0 && s.Last > s.First {
        pairs = append(pairs, [2]string{i18nl(lang).String("Activity period"), periodText(time.Duration(s.Last-s.First) * time.Second, lang)})
    }
    return pairs
}

// addrTypeText names an address form the way the Type line has always read. The
// stored records keep the form itself (what addrindex.Decode reports), so the
// wording is decided here, at the one place that displays it — and a record
// written without one, which is how an address put into the bucket by hand
// arrives, is named from the address instead, the form being a property of it.
func addrTypeText(kind, addr string) string {
    if kind == "" { _, kind, _ = addrindex.Decode(addr) }
    switch kind {
    case "p2sh":    return "script hash (P2SH)"
    case "segwit":  return "segwit (bech32)"
    case "taproot": return "taproot (P2TR)"
    }
    return "standard (P2PKH)"
}

// addrPairs builds the lines an address is described by. valid is false when the
// node says it is not an address at all, which is a different answer from the
// lookup itself failing. Shared with the Mini App's address page.
//
// An address the statistics collector follows is answered from its record and
// nothing else happens: no validateaddress, no history to resolve, no node at
// all. Those are the addresses the ranked lists make reachable by tapping, and
// they are the ones the live path below serves worst — the busiest holds
// millions of transactions against its cap of ten thousand.
//
// the same classifier the stored records are typed by, so an address reads
// the same whether it is answered from one or looked up live; a form it
// cannot decode falls back to what the node says about it
func addrPairs(ctx context.Context, lang string, addr string) ([][2]string, bool, error) {
    if s, ok := addrstat.Get(addr); ok { return statPairs(addr, s, lang), true, nil }
    if !core.Enabled() { return nil, false, errors.New("no node configured") }
    var addrInfo, err = core.ValidateAddress(ctx, addr)
    if err != nil { return nil, false, err }
    if !addrInfo.IsValid { return nil, false, nil }
    var _, kind, known = addrindex.Decode(addr)
    var addrType = addrTypeText(kind, addr)
    if !known {
        if addrInfo.IsWitness {
            addrType = "segwit (bech32)"
        } else if addrInfo.IsScript {
            addrType = "script hash (P2SH)"
        }
    }
    var pairs = [][2]string{{i18nl(lang).String("Type"), addrType}}
    var script, decodeErr = hex.DecodeString(addrInfo.ScriptPubKey)
    var txs, complete = []*core.Transaction(nil), false
    if decodeErr == nil {
        txs, complete = addressHistory(ctx, script)
    }
    if _, ok := addrindex.Cursor(); !ok {
        pairs = append(pairs, [2]string{i18nl(lang).String("Activity"), i18nl(lang).String("unavailable (address index is still building)")})
    } else if len(txs) == 0 && !complete {
        pairs = append(pairs, [2]string{i18nl(lang).String("Activity"), i18nl(lang).String("unavailable")})
    } else {
        var received, sent, fees, firstT, lastT = addressStats(txs, addr)
        var count = group(int64(len(txs)))
        if !complete { count += "+" }
        pairs = append(pairs,
            [2]string{i18nl(lang).String("Balance"), compactBTC(received - sent, lang)},
            [2]string{i18nl(lang).String("Total received"), compactBTC(received, lang)},
            [2]string{i18nl(lang).String("Total sent"), compactBTC(sent, lang)},
            [2]string{i18nl(lang).String("Total flow"), compactBTC(received + sent, lang)},
            [2]string{i18nl(lang).String("Total fees"), compactBTC(fees, lang)},
            [2]string{i18nl(lang).String("Transactions"), count},
        )
        if firstT > 0 { pairs = append(pairs, [2]string{i18nl(lang).String("First tx"), day(firstT, lang)}) }
        if lastT > 0 { pairs = append(pairs, [2]string{i18nl(lang).String("Last tx"), day(lastT, lang)}) }
        if firstT > 0 && lastT > firstT {
            pairs = append(pairs, [2]string{i18nl(lang).String("Activity period"), periodText(time.Duration(lastT-firstT) * time.Second, lang)})
        }
    }
    return pairs, true, nil
}

func address(ctx context.Context, bot *bot, chat int64, addr string) {
    var pairs, valid, err = addrPairs(ctx, chatLang(chat), addr)
    if err != nil {
        logging.Err("validate address: %v", err)
        send(bot, chat, i18n(chat).String("Sorry, something went wrong looking up that address"), nil)
        return
    }
    if !valid {
        send(bot, chat, i18n(chat).Sprintf("%s doesn't look like a valid Bitcoin address", html.EscapeString(addr)), nil)
        return
    }
    send(bot, chat, i18n(chat).Sprintf("Address %s\n\n<pre>%s</pre>", short(addr), joinAlign(pairs)), nil)
}
