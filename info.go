package main

import "context"
import "fmt"
import "html"
import "slices"
import "strconv"
import "strings"
import "sync"
import "time"
import "bitnsbot/logging"

var pendingInfoMu sync.Mutex
var pendingInfoChats = make(map[int64]bool)

func info(bot *bot, chatID int64, arg string) {
    if arg == "" {
        pendingInfoMu.Lock()
        pendingInfoChats[chatID] = true
        pendingInfoMu.Unlock()
        send(bot, chatID, "Please send Bitcoin address or transaction or block number")
        return
    }
    pendingInfoMu.Lock()
    delete(pendingInfoChats, chatID)
    pendingInfoMu.Unlock()
    if btcd == nil {
        send(bot, chatID, "Bitcoin node connection is not configured.")
        return
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if isTxid(arg) {
        transaction(ctx, bot, chatID, arg)
        return
    }
    if height, err := strconv.ParseInt(arg, 10, 64); err == nil && height >= 0 {
        block(ctx, bot, chatID, height)
        return
    }
    address(ctx, bot, chatID, arg)
}

func transaction(ctx context.Context, bot *bot, chatID int64, txid string) {
    var tx, err = btcd.getRawTransaction(ctx, txid)
    if err != nil {
        send(bot, chatID, "Couldn't find transaction "+short(txid)+".")
        return
    }
    var total float64
    for _, vout := range tx.Vout {
        total += vout.Value
    }
    var coinbase = len(tx.Vin) > 0 && tx.Vin[0].Coinbase != ""
    var fee float64
    var inputs []string
    var feeOK bool
    if !coinbase {
        fee, inputs, _, feeOK = txInputs(ctx, tx)
    }
    var pairs [][2]string
    var at = time.Time{}
    var current = true
    if tx.Confirmations == 0 {
        pairs = append(pairs, [2]string{"Status", "unconfirmed (in mempool)"})
    } else {
        at, current = time.Unix(tx.Time, 0), false
        pairs = append(pairs,
            [2]string{"Status", fmt.Sprintf("confirmed (%d confirmations)", tx.Confirmations)},
            [2]string{"Confirmed", when(tx.Time)},
            [2]string{"Block", short(tx.BlockHash)},
        )
    }
    pairs = append(pairs, [2]string{"Amount", amountLine(total, at, current)})
    switch {
    case coinbase:
        pairs = append(pairs, [2]string{"Fee", "none (coinbase)"})
    case feeOK:
        pairs = append(pairs, [2]string{"Fee", satoshi(fee) + " sats"})
    default:
        pairs = append(pairs, [2]string{"Fee", "unavailable"})
    }
    pairs = append(pairs, [2]string{"Size", group(int64(tx.Size)) + " bytes"})
    switch {
    case coinbase:
        pairs = append(pairs, [2]string{"Inputs", "coinbase (newly generated)"})
    case feeOK:
        pairs = append(pairs, [2]string{"Inputs", compactAddrs(inputs)})
    default:
        pairs = append(pairs, [2]string{"Inputs", "unavailable"})
    }
    pairs = append(pairs, [2]string{"Outputs", compactAddrs(outputAddrs(tx))})
    var pad int
    for _, p := range pairs {
        if len(p[0])+1 > pad { pad = len(p[0]) + 1 }
    }
    var lines []string
    for _, p := range pairs {
        lines = append(lines, fmt.Sprintf("%-*s %s", pad, p[0]+":", p[1]))
    }
    send(bot, chatID, fmt.Sprintf("Transaction %s\n\n<pre>%s</pre>", short(tx.Txid), strings.Join(lines, "\n")))
}

// txInputs fetches each input's prevout transaction to sum the input values (for
// the fee = inputs − outputs) and collect the spent addresses — in input order as
// addrs (for the /info listing) and summed per address as spent (which is how the
// watch notifier learns an address is *sending*, the counterpart to the receiving
// addresses it reads straight off the outputs). btcd's getrawtransaction gives
// inputs only as txid:vout refs, so the prevouts are fetched concurrently
// (bounded); any fetch failure yields ok=false so the reply shows the fee/inputs
// as unavailable rather than wrong.
func txInputs(ctx context.Context, tx *btcdTransaction) (fee float64, addrs []string, spent map[string]float64, ok bool) {
    var ids = map[string]bool{}
    for _, in := range tx.Vin {
        ids[in.Txid] = true
    }
    var prevouts = map[string]*btcdTransaction{}
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
            var p, e = btcd.getRawTransaction(ctx, id)
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
    var inSum float64
    spent = make(map[string]float64)
    for _, vin := range tx.Vin {
        var p = prevouts[vin.Txid]
        if p == nil || int(vin.Vout) >= len(p.Vout) {
            return 0, nil, nil, false
        }
        inSum += p.Vout[vin.Vout].Value
        var a = addressOf(p.Vout[vin.Vout])
        addrs = append(addrs, a)
        spent[a] += p.Vout[vin.Vout].Value
    }
    var outSum float64
    for _, v := range tx.Vout {
        outSum += v.Value
    }
    fee = inSum - outSum
    if fee < 0 {
        fee = 0
    }
    return fee, addrs, spent, true
}

func outputAddrs(tx *btcdTransaction) []string {
    var addrs []string
    for _, v := range tx.Vout {
        addrs = append(addrs, addressOf(v))
    }
    return addrs
}

func addressOf(v btcdVout) string {
    if v.ScriptPubKey.Address != "" { return v.ScriptPubKey.Address }
    if len(v.ScriptPubKey.Addresses) > 0 { return v.ScriptPubKey.Addresses[0] }
    return "(non-standard)"
}

// compactAddrs joins shortened addresses, showing at most the first three with a
// trailing "..." when there are more.
func compactAddrs(addrs []string) string {
    if len(addrs) == 0 {
        return "none"
    }
    var show, more = addrs, false
    if len(addrs) > 3 {
        show, more = addrs[:3], true
    }
    var parts []string
    for _, a := range show {
        parts = append(parts, short(a))
    }
    var s = strings.Join(parts, ", ")
    if more {
        s += ", ..."
    }
    return s
}

func block(ctx context.Context, bot *bot, chatID int64, height int64) {
    if bi, ok := loadBlock(height); ok {
        send(bot, chatID, formatBlock(bi))
        return
    }
    var hash, err = btcd.getBlockHash(ctx, height)
    if err != nil {
        send(bot, chatID, fmt.Sprintf("Couldn't find block %d.", height))
        return
    }
    var bi, ciErr = computeBlockInfo(ctx, hash)
    if ciErr != nil {
        logging.Err("compute block %d: %v", height, ciErr)
        send(bot, chatID, "Sorry, something went wrong fetching that block.")
        return
    }
    storeBlock(bi)
    send(bot, chatID, formatBlock(bi))
}

// blockFees computes each non-coinbase transaction's fee (sum of input prevout
// values minus output values) and returns the low/average/high in BTC with the
// count of fee-paying transactions. btcd's getblock omits input values, so every
// referenced prevout transaction is fetched concurrently (bounded) and cached;
// if any fetch fails (e.g. txindex disabled, or a genuinely missing prevout) it
// returns an error so the caller reports fees as unavailable rather than wrong.
func blockFees(ctx context.Context, txs []btcdBlockTx) (low, avg, high float64, count int, err error) {
    var ids = map[string]bool{}
    for _, tx := range txs {
        if len(tx.Vin) > 0 && tx.Vin[0].Coinbase != "" {
            continue
        }
        for _, in := range tx.Vin {
            ids[in.Txid] = true
        }
    }
    if len(ids) == 0 {
        return 0, 0, 0, 0, nil
    }
    var prevouts = map[string]*btcdTransaction{}
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
            var tx, e = btcd.getRawTransaction(ctx, id)
            mu.Lock()
            if e != nil {
                if fetchErr == nil { fetchErr = e }
            } else {
                prevouts[id] = tx
            }
            mu.Unlock()
        }(id)
    }
    wg.Wait()
    if fetchErr != nil {
        return 0, 0, 0, 0, fetchErr
    }
    var total float64
    for _, tx := range txs {
        if len(tx.Vin) > 0 && tx.Vin[0].Coinbase != "" {
            continue
        }
        var in, out float64
        for _, vin := range tx.Vin {
            var p = prevouts[vin.Txid]
            if p == nil || int(vin.Vout) >= len(p.Vout) {
                return 0, 0, 0, 0, fmt.Errorf("missing prevout %s:%d", vin.Txid, vin.Vout)
            }
            in += p.Vout[vin.Vout].Value
        }
        for _, vout := range tx.Vout {
            out += vout.Value
        }
        var fee = in - out
        if fee < 0 {
            fee = 0
        }
        if count == 0 {
            low, high = fee, fee
        } else {
            if fee < low { low = fee }
            if fee > high { high = fee }
        }
        total += fee
        count++
    }
    if count == 0 {
        return 0, 0, 0, 0, nil
    }
    return low, total / float64(count), high, count, nil
}

var addrTxPageSize = 1000
var addrTxLimit = 10000

// addressTxs pages through an address's transactions (oldest first), up to
// addrTxLimit. It returns the transactions and whether the set is complete —
// false when paging was cut short by the limit or an error (e.g. a timeout), so
// the caller can present the derived stats as partial. btcd reports both a
// genuinely empty history and the end of paging as the same "No information"
// error, treated here as a clean, complete end.
func addressTxs(ctx context.Context, addr string) ([]btcdAddrTx, bool) {
    var all []btcdAddrTx
    for len(all) < addrTxLimit {
        var page, err = btcd.searchAddressTxs(ctx, addr, len(all), addrTxPageSize)
        if err != nil {
            if strings.Contains(err.Error(), "No information available about address") {
                return all, true
            }
            return all, false
        }
        all = append(all, page...)
        if len(page) < addrTxPageSize {
            return all, true
        }
    }
    return all, false
}

// addressStats sums an address's on-chain history from its transactions: total
// received (outputs paying it), total sent (inputs spending from it), fees on its
// outgoing transactions (Σ inputs − Σ outputs of each tx it sends), and the
// earliest/latest confirmed transaction times.
func addressStats(txs []btcdAddrTx, addr string) (received, sent, fees float64, firstT, lastT int64) {
    for _, tx := range txs {
        for _, v := range tx.Vout {
            if v.ScriptPubKey.Address == addr || slices.Contains(v.ScriptPubKey.Addresses, addr) {
                received += v.Value
            }
        }
        var fromAddr bool
        for _, in := range tx.Vin {
            if in.PrevOut != nil && slices.Contains(in.PrevOut.Addresses, addr) {
                sent += in.PrevOut.Value
                fromAddr = true
            }
        }
        if fromAddr {
            var vinSum, voutSum float64
            var ok = true
            for _, in := range tx.Vin {
                if in.Coinbase != "" || in.PrevOut == nil { ok = false; break }
                vinSum += in.PrevOut.Value
            }
            for _, v := range tx.Vout { voutSum += v.Value }
            if ok { fees += vinSum - voutSum }
        }
        if tx.Time > 0 {
            if firstT == 0 || tx.Time < firstT { firstT = tx.Time }
            if tx.Time > lastT { lastT = tx.Time }
        }
    }
    return
}

func address(ctx context.Context, bot *bot, chatID int64, addr string) {
    var addrInfo, err = btcd.validateAddress(ctx, addr)
    if err != nil {
        logging.Err("validate address: %v", err)
        send(bot, chatID, "Sorry, something went wrong looking up that address.")
        return
    }
    if !addrInfo.IsValid {
        send(bot, chatID, html.EscapeString(addr)+" doesn't look like a valid Bitcoin address.")
        return
    }
    var addrType = "standard (P2PKH)"
    if addrInfo.IsWitness {
        addrType = "segwit (bech32)"
    } else if addrInfo.IsScript {
        addrType = "script hash (P2SH)"
    }
    var pairs = [][2]string{{"Type", addrType}}
    var txs, complete = addressTxs(ctx, addr)
    if !complete && len(txs) == 0 {
        pairs = append(pairs, [2]string{"Activity", "unavailable (is the address index enabled?)"})
    } else {
        var received, sent, fees, firstT, lastT = addressStats(txs, addr)
        var count = group(int64(len(txs)))
        if !complete { count += "+" }
        pairs = append(pairs,
            [2]string{"Balance", compactBtc(received - sent)},
            [2]string{"Total received", compactBtc(received)},
            [2]string{"Total sent", compactBtc(sent)},
            [2]string{"Total flow", compactBtc(received + sent)},
            [2]string{"Total fees", compactBtc(fees)},
            [2]string{"Transactions", count},
        )
        if firstT > 0 {
            pairs = append(pairs, [2]string{"First tx", timeCompact(firstT)})
        }
        if lastT > 0 {
            pairs = append(pairs, [2]string{"Last tx", timeCompact(lastT)})
        }
        if firstT > 0 && lastT > firstT {
            pairs = append(pairs, [2]string{"Activity period", periodText(time.Duration(lastT-firstT) * time.Second)})
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
    send(bot, chatID, fmt.Sprintf("Address %s\n\n<pre>%s</pre>", short(addr), strings.Join(lines, "\n")))
}
