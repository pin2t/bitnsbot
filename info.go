package main

import "context"
import "html"
import "encoding/hex"
import "sort"
import "strconv"
import "strings"
import "sync"
import "time"
import "bitnsbot/addrindex"
import "bitnsbot/logging"

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
    if core == nil {
        send(bot, chat, i18n(chat).String("Bitcoin node connection is not configured"), nil)
        return
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if isTxid(arg) {
        if header, err := core.getBlockHeader(ctx, arg); err == nil {
            block(ctx, bot, chat, header.Height)
            return
        }
        transaction(ctx, bot, chat, arg)
        return
    }
    if height, err := strconv.ParseInt(arg, 10, 64); err == nil && height >= 0 {
        block(ctx, bot, chat, height)
        return
    }
    address(ctx, bot, chat, arg)
}

func transaction(ctx context.Context, bot *bot, chat int64, txid string) {
    var estimates = map[string]string{
        confETAFast:   i18n(chat).String("~10-20 min"),
        confETAMedium: i18n(chat).String("~1 hour"),
        confETASlow:   i18n(chat).String("2+ hours"),
    }
    var tx, err = core.getRawTransaction(ctx, txid)
    if err != nil {
        send(bot, chat, i18n(chat).Sprintf("Couldn't find transaction %s", short(txid)), nil)
        return
    }
    var total int64
    for _, vout := range tx.Vout { total += toSat(vout.Value) }
    var coinbase = len(tx.Vin) > 0 && tx.Vin[0].Coinbase != ""
    var fee int64
    var inputs []string
    var feeOK bool
    if !coinbase { fee, inputs, _, feeOK = txInputs(ctx, tx) }
    var pairs [][2]string
    var at = time.Time{}
    var current = true
    var blockHeight int64
    if tx.Confirmations == 0 {
        var confText = i18n(chat).String("none (confirms in ~10-20 min)")
        if feeOK && tx.Vsize > 0 {
            confText = i18n(chat).String("none (confirms in") + " " + estimates[confEstimate(float64(fee) / float64(tx.Vsize))] + ")"
        }
        pairs = append(pairs, [2]string{i18n(chat).String("Confirmations"), confText})
    } else {
        at, current = time.Unix(tx.Time, 0), false
        if header, err := core.getBlockHeader(ctx, tx.BlockHash); err == nil {
            blockHeight = header.Height
        }
        pairs = append(pairs, [2]string{i18n(chat).String("Confirmations"), i18n(chat).Sprintf("%d (block #%d)", tx.Confirmations, blockHeight)})
    }
    pairs = append(pairs, [2]string{i18n(chat).String("Amount"), amountLine(total, at, current, chat)})
    if feeOK {
        var feeStr = group(fee) + " " + i18n(chat).String("sats")
        if tx.Vsize > 0 {
            var feeRate = float64(fee) / float64(tx.Vsize)
            feeStr += " (" + trimNum(feeRate, 1) + i18n(chat).String(" sat/vB") + ")"
        }
        pairs = append(pairs, [2]string{i18n(chat).String("Fee"), feeStr})
    }
    var szMsg = group(int64(tx.Size)) + " " + i18n(chat).String("B")
    if tx.Vsize > 0 { szMsg += i18n(chat).Sprintf(" (%s vB)", group(int64(tx.Vsize))) }
    pairs = append(pairs, [2]string{i18n(chat).String("Size"), szMsg})
    if feeOK {
        pairs = append(pairs, [2]string{i18n(chat).String("Inputs"), compactAddrs(inputs)})
    }
    pairs = append(pairs, [2]string{i18n(chat).String("Outputs"), compactAddrs(outputAddrs(tx))})
    // only the ids the text actually shows get buttons — compactAddrs truncates
    // to shownAddrs with a trailing "...", and a button for something the reader
    // cannot see in the message would be a puzzle rather than a shortcut
    var ids []string
    if blockHeight > 0 {
        ids = append(ids, strconv.FormatInt(blockHeight, 10))
    }
    if tx.BlockHash != "" { ids = append(ids, tx.BlockHash) }
    ids = append(ids, firstN(inputs, shownAddrs)...)
    ids = append(ids, firstN(outputAddrs(tx), shownAddrs)...)
    send(bot, chat, i18n(chat).Sprintf("Transaction <code>%s</code>\n\n<pre>%s</pre>", tx.Txid, joinAlign(pairs)), ids)
}

// txInputs reports a transaction's fee and the addresses it spends from — in
// input order as addrs (for the /info listing) and summed per address as spent
// (which is how the watch notifier learns an address is *sending*, the
// counterpart to the receiving addresses it reads straight off the outputs).
//
// Core hands the prevouts over inline at getrawtransaction verbosity 2, so a
// confirmed transaction needs no extra calls at all — the bounded prevout
// fan-out btcd forced on us is gone. That only holds for confirmed transactions
// though: a mempool transaction has no undo data, so Core omits both fee and
// prevout there and each input's previous transaction still has to be fetched.
// A fetch failure yields ok=false, so the reply degrades to "unavailable"
// rather than showing a wrong fee.
func txInputs(ctx context.Context, tx *coreTransaction) (fee int64, addrs []string, spent map[string]int64, ok bool) {
    spent = make(map[string]int64)
    var inSum int64
    var complete = true
    for _, vin := range tx.Vin {
        if vin.PrevOut == nil { complete = false; break }
        inSum += toSat(vin.PrevOut.Value)
        var a = addressOfScript(vin.PrevOut.ScriptPubKey)
        addrs = append(addrs, a)
        spent[a] += toSat(vin.PrevOut.Value)
    }
    if complete {
        if tx.Fee > 0 { return toSat(tx.Fee), addrs, spent, true }
        return inputsMinusOutputs(inSum, tx), addrs, spent, true
    }
    return fetchInputs(ctx, tx)
}

// fetchInputs is the mempool path: without undo data Core cannot supply prevouts,
// so they are fetched concurrently (bounded, the same pattern the btcd client
// used) and the fee derived from inputs − outputs.
func fetchInputs(ctx context.Context, tx *coreTransaction) (fee int64, addrs []string, spent map[string]int64, ok bool) {
    var ids = map[string]bool{}
    for _, in := range tx.Vin {
        ids[in.Txid] = true
    }
    var prevouts = map[string]*coreTransaction{}
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
            var p, e = core.getRawTransaction(ctx, id)
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
        return 0, nil, nil, false
    }
    var inSum int64
    spent = make(map[string]int64)
    for _, vin := range tx.Vin {
        var p = prevouts[vin.Txid]
        if p == nil || int(vin.Vout) >= len(p.Vout) {
            return 0, nil, nil, false
        }
        inSum += toSat(p.Vout[vin.Vout].Value)
        var a = addressOf(p.Vout[vin.Vout])
        addrs = append(addrs, a)
        spent[a] += toSat(p.Vout[vin.Vout].Value)
    }
    return inputsMinusOutputs(inSum, tx), addrs, spent, true
}

func inputsMinusOutputs(inSum int64, tx *coreTransaction) int64 {
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

func outputAddrs(tx *coreTransaction) []string {
    var addrs []string
    for _, v := range tx.Vout {
        addrs = append(addrs, addressOf(v))
    }
    return addrs
}

func addressOf(v coreVout) string { return addressOfScript(v.ScriptPubKey) }

// addressOfScript names the address an output pays. Core reports a single
// "address" field; the plural "addresses" array btcd used (and old Core versions
// emitted for bare multisig) is gone, so there is only the one field to read.
func addressOfScript(s coreScriptPubKey) string {
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
        send(bot, chat, formatBlock(bi, chat), nil)
        return
    }
    var hash, err = core.getBlockHash(ctx, height)
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
    storeBlock(bi)
    send(bot, chat, formatBlock(bi, chat), nil)
}

// feeStats summarises a block's fee distribution. Core reports each
// transaction's fee directly in getblock verbosity 2, so unlike the btcd path
// this needs no prevout fetching at all — the bounded 16-way fan-out that used
// to be here existed only because btcd made callers compute fees themselves.
// The coinbase has no fee and is skipped.
func feeStats(txs []coreTransaction) (low, avg, high int64, count int) {
    var total int64
    for i, t := range txs {
        if i == 0 { continue } // coinbase
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
func addressHistory(ctx context.Context, script []byte) (txs []*coreTransaction, complete bool) {
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
            var hash, err = core.getBlockHash(ctx, int64(h))
            if err != nil { mu.Lock(); failed = true; mu.Unlock(); return }
            var blk, berr = core.getBlockTxids(ctx, hash)
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
            var tx, err = core.getRawTransaction(ctx, id)
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

// addressStats sums an address's on-chain history from its transactions: total
// received (outputs paying it), total sent (inputs spending from it), fees on its
// outgoing transactions, and the earliest/latest confirmed transaction times.
// Confirmed transactions carry their prevouts and fee from Core directly, so both
// the sending side and the fee read straight off the transaction.
func addressStats(txs []*coreTransaction, addr string) (received, sent, fees int64, firstT, lastT int64) {
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

func address(ctx context.Context, bot *bot, chat int64, addr string) {
    var addrInfo, err = core.validateAddress(ctx, addr)
    if err != nil {
        logging.Err("validate address: %v", err)
        send(bot, chat, i18n(chat).String("Sorry, something went wrong looking up that address"), nil)
        return
    }
    if !addrInfo.IsValid {
        send(bot, chat, i18n(chat).Sprintf("%s doesn't look like a valid Bitcoin address", html.EscapeString(addr)), nil)
        return
    }
    var addrType = "standard (P2PKH)"
    if addrInfo.IsWitness {
        addrType = "segwit (bech32)"
    } else if addrInfo.IsScript {
        addrType = "script hash (P2SH)"
    }
    var pairs = [][2]string{{i18n(chat).String("Type"), addrType}}
    var script, decodeErr = hex.DecodeString(addrInfo.ScriptPubKey)
    var txs, complete = []*coreTransaction(nil), false
    if decodeErr == nil {
        txs, complete = addressHistory(ctx, script)
    }
    if _, ok := addrindex.Cursor(); !ok {
        pairs = append(pairs, [2]string{i18n(chat).String("Activity"), i18n(chat).String("unavailable (address index is still building)")})
    } else if len(txs) == 0 && !complete {
        pairs = append(pairs, [2]string{i18n(chat).String("Activity"), i18n(chat).String("unavailable")})
    } else {
        var received, sent, fees, firstT, lastT = addressStats(txs, addr)
        var count = group(int64(len(txs)))
        if !complete { count += "+" }
        pairs = append(pairs,
            [2]string{i18n(chat).String("Balance"), compactBTC(received - sent, chat)},
            [2]string{i18n(chat).String("Total received"), compactBTC(received, chat)},
            [2]string{i18n(chat).String("Total sent"), compactBTC(sent, chat)},
            [2]string{i18n(chat).String("Total flow"), compactBTC(received + sent, chat)},
            [2]string{i18n(chat).String("Total fees"), compactBTC(fees, chat)},
            [2]string{i18n(chat).String("Transactions"), count},
        )
        if firstT > 0 { pairs = append(pairs, [2]string{i18n(chat).String("First tx"), when(firstT, chat)}) }
        if lastT > 0 { pairs = append(pairs, [2]string{i18n(chat).String("Last tx"), when(lastT, chat)}) }
        if firstT > 0 && lastT > firstT {
            pairs = append(pairs, [2]string{i18n(chat).String("Activity period"), periodText(time.Duration(lastT-firstT) * time.Second, chat)})
        }
    }
    send(bot, chat, i18n(chat).Sprintf("Address %s\n\n<pre>%s</pre>", short(addr), joinAlign(pairs)), nil)
}
