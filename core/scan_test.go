package core_test

import "context"
import "os"
import "testing"
import "time"
import "bitnsbot/core"

// TestScanTxOutSetTiming measures scantxoutset against a real node for 10, 100,
// 1000 and 10000 addresses at once, printing how long each scan took and what
// it found. It is skipped unless CORE_URL names a node, with CORE_COOKIE (or
// CORE_USER and CORE_PASS) its credentials; each scan walks the whole UTXO set,
// minutes on mainnet, so run it with a long -timeout:
//
//     CORE_URL=http://127.0.0.1:8332 CORE_COOKIE=~/.bitcoin/.cookie \
//         go test -run TestScanTxOutSetTiming -v -timeout 2h ./core/
//
// The addresses are the ones paid by the newest blocks, walking back from the
// tip until there are enough distinct ones, so most of them hold coins now.
func TestScanTxOutSetTiming(t *testing.T) {
    var url = os.Getenv("CORE_URL")
    if url == "" { t.Skip("CORE_URL is not set") }
    if err := core.Init(url, os.Getenv("CORE_USER"), os.Getenv("CORE_PASS"), os.Getenv("CORE_COOKIE")); err != nil { t.Fatal(err) }
    t.Cleanup(core.Reset)
    var ctx = context.Background()
    var tip, err = core.GetBlockCount(ctx)
    if err != nil { t.Fatal(err) }
    var counts = []int{10, 100, 1000, 10000}
    var want = counts[len(counts)-1]
    var seen = map[string]bool{}
    var addrs []string
    for height := tip; len(addrs) < want && height >= 0; height-- {
        var hash, hashErr = core.GetBlockHash(ctx, height)
        if hashErr != nil { t.Fatal(hashErr) }
        var blk, blkErr = core.GetBlockVerbose(ctx, hash)
        if blkErr != nil { t.Fatal(blkErr) }
        for _, tx := range blk.Tx {
            for _, out := range tx.Vout {
                var a = out.ScriptPubKey.Address
                if a == "" || seen[a] || len(addrs) == want { continue }
                seen[a] = true
                addrs = append(addrs, a)
            }
        }
        t.Logf("block %d: %d addresses so far", height, len(addrs))
    }
    if len(addrs) < want { t.Fatalf("only %d addresses on the chain", len(addrs)) }
    for _, n := range counts {
        var started = time.Now()
        var res, scanErr = core.ScanTxOutSet(ctx, addrs[:n])
        var took = time.Since(started)
        if scanErr != nil { t.Fatalf("%d addresses: %v after %s", n, scanErr, took) }
        var total float64
        for _, u := range res.Unspents { total += u.Amount }
        t.Logf("%5d addresses: %s, %d unspents, %.8f BTC", n, took.Round(time.Millisecond), len(res.Unspents), total)
    }
}
