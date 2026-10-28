package blocks

import "context"
import "fmt"
import "path/filepath"
import "strconv"
import "strings"
import "testing"
import "database/sql"
import _ "modernc.org/sqlite"
import "bitnsbot/core/coretest"
import "bitnsbot/cursors"
import "bitnsbot/miners"

// open gives the package a fresh database, and miners and cursors the same one:
// Compute attributes the coinbase through miners, and Update must keep no place
// in cursors.
func open(t *testing.T) *sql.DB {
    t.Helper()
    var handle, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "blocks.db"))
    if err != nil { t.Fatal(err) }
    if err := cursors.Init(handle); err != nil { t.Fatalf("cursors: %v", err) }
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    if err := miners.Init(handle); err != nil { t.Fatalf("miners: %v", err) }
    t.Cleanup(func() { handle.Close(); db = nil; miners.Init(nil); cursors.Init(nil) })
    return handle
}

func TestSubsidy(t *testing.T) {
    var cases = map[int64]int64{0: 5000000000, 209999: 5000000000, 210000: 2500000000, 420000: 1250000000, 630000: 625000000, 840000: 312500000}
    for h, want := range cases {
        if got := Subsidy(h); got != want {
            t.Fatalf("Subsidy(%d) = %v, want %v", h, got, want)
        }
    }
}

// Supply is summed per halving epoch, so the epoch boundaries are where it can
// go wrong. Values are the issuance schedule, in satoshi.
//
// genesis alone: 50 BTC
//
// two blocks at 50
//
// all of epoch 0: 210000 x 50 = 10.5M BTC
//
// first block of epoch 1 pays 25
//
// 10.5M + 5.25M + 2.625M = 18.375M BTC
//
// and it must never exceed the 21M cap, however far out we look
func TestCirculatingSupply(t *testing.T) {
    var cases = []struct {
        height int64
        want   int64
    }{
        {0, 5000000000},
        {1, 10000000000},
        {209999, 1050000000000000},
        {210000, 1050002500000000},
        {629999, 1837500000000000},
    }
    for _, c := range cases {
        if got := CirculatingSupply(c.height); got != c.want {
            t.Errorf("CirculatingSupply(%d) = %d, want %d", c.height, got, c.want)
        }
    }
    if got := CirculatingSupply(100000000); got > 2100000000000000 {
        t.Errorf("supply at a far-future height = %d, above the 21M cap", got)
    }
}

func TestFlushLoad(t *testing.T) {
    open(t)
    var bi = &Info{Height: 700000, Hash: "abc", Miner: "PoolX", NumTx: 5, Reward: 625000000}
    if err := Flush([]*Info{bi}); err != nil {
        t.Fatalf("store: %v", err)
    }
    var got, ok = Load(700000)
    if !ok || got.Miner != "PoolX" || got.NumTx != 5 || got.Height != 700000 {
        t.Fatalf("load: %+v ok=%v", got, ok)
    }
    if _, ok := Load(999999); ok {
        t.Fatalf("expected miss for uncached height")
    }
}

// Rollback removes the disconnected block and everything built on it, leaves
// the blocks below alone, and does nothing for a hash it does not hold.
func TestRollback(t *testing.T) {
    open(t)
    var bis []*Info
    for h := int64(100); h <= 104; h++ { bis = append(bis, &Info{Height: h, Hash: fmt.Sprintf("hash%d", h)}) }
    if err := Flush(bis); err != nil { t.Fatal(err) }
    var n, err = Rollback("unknown")
    if err != nil || n != 0 { t.Fatalf("unknown hash: removed %d, %v", n, err) }
    n, err = Rollback("hash102")
    if err != nil || n != 3 { t.Fatalf("removed %d, %v; want 3", n, err) }
    for h := int64(100); h <= 104; h++ {
        if _, ok := Load(h); ok != (h < 102) { t.Errorf("block %d cached = %v", h, ok) }
    }
    var plan string
    if err := db.QueryRow("explain query plan select height from blocks where hash = ?", "x").Scan(
        new(int), new(int), new(int), &plan); err != nil {
        t.Fatal(err)
    }
    if !strings.Contains(plan, "blocks_hash") { t.Errorf("a lookup by hash does not use blocks_hash: %s", plan) }
}

// a pool record the way the definitions are stored, then a second Init so the
// package rebuilds the address→pool mapping it attributes from
//
// coinbase (out 50.0015 = reward 50 + fees 0.0015) + two fee-paying txs
//
// fee 0.001
//
// fee 0.0005
//
// Core supplies each fee directly, so no prevout fetching happens at all
//
// (100+200+250)/3 = 183
//
// 50 BTC and 50.0015 BTC
func TestCompute(t *testing.T) {
    var handle = open(t)
    for _, q := range []string{
        `insert into miners (name, blocks, reward, fees, totalWork, lastWork) values ('TestPool', 0, 0, 0, 0, 0)`,
        `insert into mineraddr (address, name) values ('mineraddr', 'TestPool')`,
    } {
        if _, err := handle.Exec(q); err != nil { t.Fatalf("seed miners: %v", err) }
    }
    if err := miners.Init(handle); err != nil { t.Fatalf("reload miners: %v", err) }
    var srv = coretest.Server(t, func(method string, params []any) (any, error) {
        var p = params
        _ = p
        switch method {
        case "getblock":
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
                return map[string]any{"vout": []map[string]any{{"value": 1.001}}}, nil
            case "prevb":
                return map[string]any{"vout": []map[string]any{{"value": 0.5}, {"value": 2.0005}}}, nil
            }
            return nil, fmt.Errorf("no such tx")
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    defer srv.Close()
    coretest.Use(t, srv)
    var bi, err = Compute(context.Background(), "hash500")
    if err != nil {
        t.Fatalf("Compute: %v", err)
    }
    if bi.Height != 500 || bi.Size != 550 || bi.NumTx != 3 {
        t.Fatalf("general fields: %+v", bi)
    }
    if bi.Miner != "TestPool" {
        t.Fatalf("miner = %q, want TestPool", bi.Miner)
    }
    if !bi.FeesOK || bi.FeeMin != 50000 || bi.FeeAvg != 75000 || bi.FeeMax != 100000 {
        t.Fatalf("fees: ok=%v min=%v avg=%v max=%v", bi.FeesOK, bi.FeeMin, bi.FeeAvg, bi.FeeMax)
    }
    if bi.TxSizeMin != 100 || bi.TxSizeAvg != 183 || bi.TxSizeMax != 250 {
        t.Fatalf("tx sizes: min=%d avg=%d max=%d", bi.TxSizeMin, bi.TxSizeAvg, bi.TxSizeMax)
    }
    if bi.Reward != 5000000000 || bi.Total != 5000150000 {
        t.Fatalf("reward=%v total=%v", bi.Reward, bi.Total)
    }
}

// A block arriving over ZMQ wakes the index catch-up goroutine, which zmq.go
// signals rather than computing the block itself: Update walks from the highest
// block stored to the tip it was handed, so it stores the new block and any the
// bot was down for, in height order — keeping no place in cursors, since its
// place is the blocks table.
//
// one chunk, so the whole 0..100 catch-up is a single pass
//
// the next pass resumes past the highest block stored, fetching only what is new
func TestUpdate(t *testing.T) {
    open(t)
    var tip = 100
    var fetched []int64
    var srv = coretest.Server(t, func(method string, params []any) (any, error) {
        switch method {
        case "getblockhash":
            fetched = append(fetched, int64(params[0].(float64)))
            return fmt.Sprintf("0000000000000000abc%v", params[0]), nil
        case "getblock":
            var height, _ = strconv.Atoi(strings.TrimPrefix(params[0].(string), "0000000000000000abc"))
            return map[string]any{"hash": params[0], "height": height, "time": 1700000000, "size": 300,
                "tx": []map[string]any{{"txid": "cb", "size": 100, "vin": []map[string]any{{"coinbase": "03"}}, "vout": []map[string]any{{"value": 50.0}}}}}, nil
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    defer srv.Close()
    coretest.Use(t, srv)
    var saved = chunkSize
    chunkSize = 1000
    defer func() { chunkSize = saved }()
    Update(int64(tip))
    if _, ok := Load(100); !ok {
        t.Fatal("the tip was not cached by the catch-up a block notification runs")
    }
    if _, ok := Load(0); !ok {
        t.Error("the catch-up skipped the blocks below the tip")
    }
    if _, ok := cursors.Get("blocks"); ok {
        t.Error("the collector kept a place in cursors; its place is the blocks table")
    }
    tip, fetched = 102, nil
    Update(int64(tip))
    if len(fetched) != 2 || fetched[0] != 101 || fetched[1] != 102 {
        t.Errorf("second pass fetched %v, want [101 102]", fetched)
    }
}

// A database written before unattributed blocks stored an empty miner carries the
// literal "Unknown"; Init normalises it, so the placeholder is applied at
// render rather than baked into the data.
func TestInitMigratesUnknownMiner(t *testing.T) {
    var handle = open(t)
    if err := Flush([]*Info{
        {Height: 800001, Hash: "h1", Miner: "Unknown"},
        {Height: 800002, Hash: "h2", Miner: "AntPool"},
    }); err != nil {
        t.Fatalf("store blocks: %v", err)
    }
    if err := Init(handle); err != nil { t.Fatalf("re-init: %v", err) }
    var bi, ok = Load(800001)
    if !ok || bi.Miner != "" {
        t.Errorf("legacy Unknown miner = %q, want empty after the migration", bi.Miner)
    }
    if bi, ok = Load(800002); !ok || bi.Miner != "AntPool" {
        t.Errorf("an attributed miner must survive the migration, got %q", bi.Miner)
    }
}
