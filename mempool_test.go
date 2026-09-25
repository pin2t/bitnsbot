package main

import "encoding/hex"
import "fmt"
import "strings"
import "testing"
import "time"
import "bitnsbot/core/coretest"

// The zmq fixtures are named after their txids, so the local hash has to land
// on the name Core gave each — a segwit spend, a three-output send and a
// coinbase — and the outputs' total on what they pay.
func TestParseTxTxidAndAmount(t *testing.T) {
    var cases = []struct {
        prefix   string
        raw      string
        amount   int64
        coinbase bool
    }{
        {"8ecde571dbd8", "020000000001011f9abe49ce4af64ac634ee3b151a2f2ac86533d7b8d007d2b8101867b4d647bd0000000000fdffffff02bcd6e40000000000160014f31640a577726488ea712f4b570e758bbe0cea508096980000000000160014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d802473044022033ee7f6288732a9505c2782fdacdbb20288cfbf2ccc63e26f95fd5fa2640445c02202e1e1cf91fafbb36a8b82d675b1bf90b7780b79b74dc2744884a75b40d0b191d012102be5d9890238a0cdbfdd3f421df030908f78b09b93f911f6c20d01058ecc478c167000000", 0xe4d6bc + 0x989680, false},
        {"01d2a3f7f8e3", "02000000000101a7ba6e78ddb6a979b4696ebeeb0c978094a627517ea4455f89b682867c7906270100000000fdffffff030cd8fa02000000001600146c365b76d169121a1220b2e472f4c4f4178d3036002d310100000000160014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d880c3c90100000000160014a95b146a45ea18e4d17d6ef35d99ea8217ff54d80247304402203c60b7d839f8688a4871d040ff93f885bacee49ff39829e9da60f7cfa7635fed02200f179c6198ae5a7ab4d94e51e9e0ddd8dbf73294e2de1cb60db9a58d7a67eaae012103fd4effc1cc3f2da8587fa1c1d36f5f24fb326b3673888caf1c68335a614b07b767000000", 0x02fad80c + 0x01312d00 + 0x01c9c380, false},
        {"faee5caba920", "020000000001010000000000000000000000000000000000000000000000000000000000000000ffffffff03016800feffffff02740a062a01000000160014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d80000000000000000266a24aa21a9ede887852b50c23289795117c3ab5de19d8ec063f544be15b2dff04001cbf312000120000000000000000000000000000000000000000000000000000000000000000067000000", 0x012a060a74, true},
    }
    for _, c := range cases {
        var raw, _ = hex.DecodeString(c.raw)
        var tx, ok = parseTx(raw)
        if !ok { t.Fatalf("%s: parseTx failed", c.prefix) }
        if id := txid(raw, tx); !strings.HasPrefix(id, c.prefix) || len(id) != 64 {
            t.Errorf("%s: txid = %s", c.prefix, id)
        }
        if tx.amount != c.amount {
            t.Errorf("%s: amount = %d, want %d", c.prefix, tx.amount, c.amount)
        }
        if tx.coinbase != c.coinbase {
            t.Errorf("%s: coinbase = %v, want %v", c.prefix, tx.coinbase, c.coinbase)
        }
        if txid(raw[:tx.outputsEnd], tx) != "" {
            t.Errorf("%s: a txid from a transaction cut before its locktime", c.prefix)
        }
    }
}

// legacyTx is a one-input, one-output pre-segwit transaction whose outputs pay
// sat and whose input spends a distinct outpoint per n, so each has its own txid.
func legacyTx(n int, sat int64) []byte {
    var b strings.Builder
    b.WriteString("01000000" + "01")
    b.WriteString(fmt.Sprintf("%064x", n) + "00000000" + "00" + "ffffffff")
    var amount = make([]byte, 8)
    for i := 0; i < 8; i++ { amount[i] = byte(sat >> (8 * i)) }
    b.WriteString("01" + hex.EncodeToString(amount) + "016a" + "00000000")
    var raw, _ = hex.DecodeString(b.String())
    return raw
}

func record(t *testing.T, raw []byte) string {
    t.Helper()
    var tx, ok = parseTx(raw)
    if !ok { t.Fatalf("parseTx failed on %x", raw) }
    recordMempoolTx(raw, tx)
    return txid(raw, tx)
}

func TestMempoolListRecordsArrivals(t *testing.T) {
    resetMempool()
    t.Cleanup(resetMempool)
    var first = record(t, legacyTx(1, 12000))
    var second = record(t, legacyTx(2, 25_000_000))
    var mp = appSource{}.Mempool("", 0)
    if len(mp.Txs) != 2 || mp.Txs[0].Id != second || mp.Txs[1].Id != first {
        t.Fatalf("list = %+v, want the two arrivals newest first", mp.Txs)
    }
    if mp.Txs[1].Amount != "12 000 sats" || mp.Txs[0].Amount != "0.25 BTC" {
        t.Errorf("amounts = %q, %q", mp.Txs[1].Amount, mp.Txs[0].Amount)
    }
    if mp.Txs[0].Short != short(second) {
        t.Errorf("short = %q", mp.Txs[0].Short)
    }
    if mp.Top != 2 {
        t.Errorf("Top = %d, want 2", mp.Top)
    }
    var newer = appSource{}.Mempool("", 1)
    if len(newer.Txs) != 1 || newer.Txs[0].Id != second || newer.More {
        t.Errorf("after 1 = %+v, want the second arrival alone", newer)
    }
    for i := 3; i <= mempoolKept+5; i++ { record(t, legacyTx(i, 1000)) }
    if mp = (appSource{}).Mempool("", 0); len(mp.Txs) != mempoolKept {
        t.Errorf("list holds %d, want %d", len(mp.Txs), mempoolKept)
    }
}

// A coinbase starts a block's republished transactions: they come off the list
// rather than going on it, and an open page is told which rows to drop.
func TestMempoolListDropsMinedTransactions(t *testing.T) {
    resetMempool()
    t.Cleanup(resetMempool)
    var first = record(t, legacyTx(1, 1000))
    var second = record(t, legacyTx(2, 2000))
    var coinbase, _ = hex.DecodeString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0100ffffffff0100f2052a01000000016a00000000")
    record(t, coinbase)
    record(t, legacyTx(1, 1000))
    record(t, legacyTx(99, 1000))
    var mp = appSource{}.Mempool("", 0)
    if len(mp.Txs) != 1 || mp.Txs[0].Id != second {
        t.Fatalf("list = %+v, want only the unmined arrival", mp.Txs)
    }
    if len(mp.Gone) != 1 || mp.Gone[0] != first {
        t.Errorf("gone = %v, want %s", mp.Gone, first)
    }
    if gone := (appSource{}).Mempool("", mp.Top).Gone; len(gone) != 0 {
        t.Errorf("gone after the page caught up = %v", gone)
    }
    mempoolMu.Lock()
    mempoolMinedUntil = time.Time{}
    mempoolMu.Unlock()
    var third = record(t, legacyTx(3, 1000))
    if mp = (appSource{}).Mempool("", 0); mp.Txs[0].Id != third {
        t.Errorf("an arrival after the block's burst is not listed")
    }
}

// A page that fell further behind than the list keeps is handed the whole list.
func TestMempoolOverflowIsTheWholeList(t *testing.T) {
    resetMempool()
    t.Cleanup(resetMempool)
    for i := 1; i <= mempoolKept+10; i++ { record(t, legacyTx(i, 1000)) }
    var mp = appSource{}.Mempool("", 3)
    if !mp.More || len(mp.Txs) != mempoolKept {
        t.Errorf("More = %v with %d rows, want the whole list", mp.More, len(mp.Txs))
    }
}

func TestMempoolFields(t *testing.T) {
    resetMempool()
    t.Cleanup(resetMempool)
    coretest.Start(t, func(method string, params []interface{}) (interface{}, error) {
        return map[string]interface{}{"size": 36552, "bytes": 12500000}, nil
    })
    flowMu.Lock()
    flowRate, flowRateOK, flowChangeOK = 4.2, true, false
    flowMu.Unlock()
    t.Cleanup(func() { flowMu.Lock(); flowRateOK = false; flowMu.Unlock() })
    var mp = appSource{}.Mempool("ru", -1)
    if !mp.OK {
        t.Fatalf("page not OK: %+v", mp)
    }
    var got = map[string]string{}
    for _, f := range mp.Rows { got[f.Label] = f.Value }
    if got["Размер"] != "12.5 МБ" || got["Транзакций"] != "36 552" || got["Скорость поступления"] != "4.2 тр/сек" {
        t.Errorf("fields = %v", got)
    }
}
