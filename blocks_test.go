package main

import "context"
import "fmt"
import "path/filepath"
import "strings"
import "testing"
import "time"

import "go.etcd.io/bbolt"

func TestSubsidy(t *testing.T) {
    var cases = map[int64]int64{0: 5000000000, 209999: 5000000000, 210000: 2500000000, 420000: 1250000000, 630000: 625000000, 840000: 312500000}
    for h, want := range cases {
        if got := subsidy(h); got != want {
            t.Fatalf("subsidy(%d) = %v, want %v", h, got, want)
        }
    }
}

func TestStoreLoadBlock(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var bi = &blockInfo{Height: 700000, Hash: "abc", Miner: "PoolX", NumTx: 5, Reward: 625000000}
    if err := storeBlock(bi); err != nil {
        t.Fatalf("store: %v", err)
    }
    var got, ok = loadBlock(700000)
    if !ok || got.Miner != "PoolX" || got.NumTx != 5 || got.Height != 700000 {
        t.Fatalf("load: %+v ok=%v", got, ok)
    }
    if _, ok := loadBlock(999999); ok {
        t.Fatalf("expected miss for uncached height")
    }
}

func TestComputeBlockInfo(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "miners.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    if err := db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("miners")).Put([]byte("mineraddr"), []byte("TestPool"))
    }); err != nil {
        t.Fatalf("seed miners: %v", err)
    }
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        var p = params
        _ = p
        switch method {
        case "getblock":
            // coinbase (out 50.0015 = reward 50 + fees 0.0015) + two fee-paying txs
            return map[string]any{
                "hash": "hash500", "height": 500, "time": 1700000000, "size": 550, "difficulty": 2.0,
                "tx": []map[string]any{
                    {"txid": "cb", "size": 100, "vin": []map[string]any{{"coinbase": "03abcd"}},
                        "vout": []map[string]any{{"value": 50.0015, "scriptPubKey": map[string]any{"address": "mineraddr"}}}},
                    {"txid": "txa", "size": 200, "fee": 0.0005, "vin": []map[string]any{{"txid": "preva", "vout": 0}}, "vout": []map[string]any{{"value": 1.0}}},
                    {"txid": "txb", "size": 250, "fee": 0.001, "vin": []map[string]any{{"txid": "prevb", "vout": 1}}, "vout": []map[string]any{{"value": 2.0}}},
                },
            }, nil
        case "getrawtransaction":
            switch id, _ := p[0].(string); id {
            case "preva":
                return map[string]any{"vout": []map[string]any{{"value": 1.001}}}, nil // fee 0.001
            case "prevb":
                return map[string]any{"vout": []map[string]any{{"value": 0.5}, {"value": 2.0005}}}, nil // fee 0.0005
            }
            return nil, fmt.Errorf("no such tx")
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    defer srv.Close()
    core = newFakeCoreConn(t, srv)
    defer func() { core = nil }()
    var bi, err = computeBlockInfo(context.Background(), "hash500")
    if err != nil {
        t.Fatalf("computeBlockInfo: %v", err)
    }
    if bi.Height != 500 || bi.Size != 550 || bi.NumTx != 3 {
        t.Fatalf("general fields: %+v", bi)
    }
    if bi.Miner != "TestPool" {
        t.Fatalf("miner = %q, want TestPool", bi.Miner)
    }
    // Core supplies each fee directly, so no prevout fetching happens at all
    if !bi.FeesOK || group(bi.FeeMin) != "50 000" || group(bi.FeeAvg) != "75 000" || group(bi.FeeMax) != "100 000" {
        t.Fatalf("fees: ok=%v min=%v avg=%v max=%v", bi.FeesOK, bi.FeeMin, bi.FeeAvg, bi.FeeMax)
    }
    if bi.TxSizeMin != 100 || bi.TxSizeAvg != 183 || bi.TxSizeMax != 250 { // (100+200+250)/3 = 183
        t.Fatalf("tx sizes: min=%d avg=%d max=%d", bi.TxSizeMin, bi.TxSizeAvg, bi.TxSizeMax)
    }
    if group(bi.Reward) != "5 000 000 000" || group(bi.Total) != "5 000 150 000" { // 50 BTC and 50.0015 BTC
        t.Fatalf("reward=%v total=%v", bi.Reward, bi.Total)
    }
}

func TestFormatBlock(t *testing.T) {
    var bi = &blockInfo{
        Height: 800000, Hash: "00000000000000000000abcdef1122334455667788fedcba", Time: 1700000000,
        Size: 1523456, NumTx: 2, Miner: "Foundry USA", FeesOK: true,
        FeeMin: 130, FeeAvg: 18500, FeeMax: 2500000,
        TxSizeMin: 110, TxSizeAvg: 445, TxSizeMax: 98000,
        Reward: 312500000, Total: 375500000, Difficulty: 79000000000000,
    }
    var s = formatBlock(bi, 1)
    for _, want := range []string{
        "Block #800000",
        "Size:          1.52 M",
        "Transactions:  2",
        "Miner:         Foundry USA",
        "Difficulty:    79 T",
        "lowest:        130 sats (1.2 sat/vB)",
        "average:       18 500 sats (41.6 sat/vB)",
        "highest:       2 500 000 sats (25.5 sat/vB)",
        "Reward:        3.125 BTC",
        "Reward + fees: 3.755 BTC</pre>",
    } {
        if !strings.Contains(s, want) {
            t.Fatalf("formatBlock missing %q in:\n%s", want, s)
        }
    }
}

func TestBlockNotification(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        var p = params
        _ = p
        switch method {
        case "getblock":
            return map[string]any{"hash": "0000000000000000abc", "height": 100, "time": 1700000000, "size": 300,
                "tx": []map[string]any{{"txid": "cb", "size": 100, "vin": []map[string]any{{"coinbase": "03"}}, "vout": []map[string]any{{"value": 50.0}}}}}, nil
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    defer srv.Close()
    core = newFakeCoreConn(t, srv)
    defer func() { core = nil }()
    // Core pushes new tips over ZMQ rather than through an RPC subscription, so
    // this is what zmq.go does on a hashblock frame.
    go processBlock("0000000000000000abc")
    var ok bool
    for i := 0; i < 40 && !ok; i++ {
        _, ok = loadBlock(100)
        time.Sleep(50 * time.Millisecond)
    }
    if !ok {
        t.Fatalf("block 100 was not cached from the blockconnected notification")
    }
}
