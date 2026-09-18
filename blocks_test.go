package main

import "context"
import "encoding/json"
import "fmt"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "strconv"
import "strings"
import "testing"
import "bitnsbot/core/coretest"
import "bitnsbot/app"
import "bitnsbot/cursors"
import "bitnsbot/miners"

func TestSubsidy(t *testing.T) {
    var cases = map[int64]int64{0: 5000000000, 209999: 5000000000, 210000: 2500000000, 420000: 1250000000, 630000: 625000000, 840000: 312500000}
    for h, want := range cases {
        if got := subsidy(h); got != want {
            t.Fatalf("subsidy(%d) = %v, want %v", h, got, want)
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
        if got := circulatingSupply(c.height); got != c.want {
            t.Errorf("circulatingSupply(%d) = %d, want %d", c.height, got, c.want)
        }
    }
    if got := circulatingSupply(100000000); got > 2100000000000000 {
        t.Errorf("supply at a far-future height = %d, above the 21M cap", got)
    }
}

func TestFlushLoadBlock(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var bi = &blockInfo{Height: 700000, Hash: "abc", Miner: "PoolX", NumTx: 5, Reward: 625000000}
    if err := flushBlocks([]*blockInfo{bi}); err != nil {
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
func TestComputeBlockInfo(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "miners.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    for _, q := range []string{
        `insert into miners (name, blocks, reward, fees, totalWork, lastWork) values ('TestPool', 0, 0, 0, 0, 0)`,
        `insert into mineraddr (address, name) values ('mineraddr', 'TestPool')`,
    } {
        if _, err := db.Exec(q); err != nil { t.Fatalf("seed miners: %v", err) }
    }
    if err := miners.Init(db); err != nil { t.Fatalf("reload miners: %v", err) }
    var srv = coretest.Server(t, func(method string, params []interface{}) (interface{}, error) {
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
    if !bi.FeesOK || group(bi.FeeMin) != "50 000" || group(bi.FeeAvg) != "75 000" || group(bi.FeeMax) != "100 000" {
        t.Fatalf("fees: ok=%v min=%v avg=%v max=%v", bi.FeesOK, bi.FeeMin, bi.FeeAvg, bi.FeeMax)
    }
    if bi.TxSizeMin != 100 || bi.TxSizeAvg != 183 || bi.TxSizeMax != 250 {
        t.Fatalf("tx sizes: min=%d avg=%d max=%d", bi.TxSizeMin, bi.TxSizeAvg, bi.TxSizeMax)
    }
    if group(bi.Reward) != "5 000 000 000" || group(bi.Total) != "5 000 150 000" {
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
    var s = formatBlock(bi, "")
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

// A block arriving over ZMQ is cached by the collector, which zmq.go wakes with
// a signal rather than computing the block itself: it walks from the highest
// block stored to the tip, so it stores the new block and any the bot was down
// for, in height order.
//
// one chunk, so the whole 0..100 catch-up is a single pass
//
// the next pass resumes past the highest block stored, fetching only what is new
func TestBlockNotification(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var tip = 100
    var fetched []int64
    var srv = coretest.Server(t, func(method string, params []interface{}) (interface{}, error) {
        switch method {
        case "getblockcount":
            return tip, nil
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
    var saved = blocksChunkSize
    blocksChunkSize = 1000
    defer func() { blocksChunkSize = saved }()
    collectBlocks()
    if _, ok := loadBlock(100); !ok {
        t.Fatal("the tip was not cached by the catch-up a block notification runs")
    }
    if _, ok := loadBlock(0); !ok {
        t.Error("the catch-up skipped the blocks below the tip")
    }
    if _, ok := cursors.Get("blocks"); ok {
        t.Error("the collector kept a place in cursors; its place is the blocks table")
    }
    tip, fetched = 102, nil
    collectBlocks()
    if len(fetched) != 2 || fetched[0] != 101 || fetched[1] != 102 {
        t.Errorf("second pass fetched %v, want [101 102]", fetched)
    }
}

// A block the collector has not reached is computed for /info and for the Mini
// App's block page, and shown, but not stored: the highest height stored is where
// the collector resumes, so a lookup above it would step the collector over every
// block in between.
func TestBlockLookupDoesNotStore(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var srv = coretest.Server(t, func(method string, params []interface{}) (interface{}, error) {
        switch method {
        case "getblockhash":
            return "0000000000000000abc500", nil
        case "getblock":
            return map[string]any{"hash": params[0], "height": 500, "time": 1700000000, "size": 300,
                "tx": []map[string]any{{"txid": "cb", "size": 100, "vin": []map[string]any{{"coinbase": "03"}}, "vout": []map[string]any{{"value": 50.0}}}}}, nil
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    coretest.Use(t, srv)
    var sent []string
    var tg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct{ Text string `json:"text"` }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, body.Text)
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer tg.Close()
    block(context.Background(), newBot("TESTTOKEN", tg.URL), 1, 500)
    if len(sent) != 1 || !strings.Contains(sent[0], "Block #500") {
        t.Fatalf("/info 500 replied %q", sent)
    }
    if info := (appSource{}).BlockInfo("", 500); !info.OK {
        t.Fatal("the Mini App's block page did not show a block it had to compute")
    }
    if _, ok := loadBlock(500); ok {
        t.Error("a looked-up block was stored; only the collector fills the cache")
    }
    var n int
    if err := db.QueryRow("select count(*) from blocks").Scan(&n); err != nil { t.Fatal(err) }
    if n != 0 {
        t.Errorf("%d rows in blocks after two lookups, want 0", n)
    }
}

// The collector used to keep its place in cursors. A row left there by that
// version is dropped on open, rather than sitting beside the table that is now
// the place and disagreeing with it.
func TestOpenDBDropsTheOldBlocksCursor(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "watches.db")
    if err := openDB(path); err != nil { t.Fatalf("openDB: %v", err) }
    if _, err := db.Exec("insert into cursors (name, place) values ('blocks', 965002), ('miners', 7)"); err != nil { t.Fatal(err) }
    closeDB()
    if err := openDB(path); err != nil { t.Fatalf("reopen: %v", err) }
    defer closeDB()
    if v, ok := cursors.Get("blocks"); ok {
        t.Errorf("the old blocks cursor survived at %d", v)
    }
    if v, ok := cursors.Get(cursors.Miners); !ok || v != 7 {
        t.Errorf("miners cursor = %d (found %v), want 7 — only the blocks row goes", v, ok)
    }
}

// An empty miner is the unattributed placeholder, in the reader's language; a
// real pool name passes through untouched.
func TestMinerName(t *testing.T) {
    if got := minerName("", ""); got != "Unknown" {
        t.Errorf(`minerName("", "") = %q, want the English placeholder`, got)
    }
    if got := minerName("", "ru"); got != "неизвестен" {
        t.Errorf(`minerName("", "ru") = %q, want the Russian placeholder`, got)
    }
    if got := minerName("AntPool", "ru"); got != "AntPool" {
        t.Errorf(`minerName("AntPool", "ru") = %q, want the name unchanged`, got)
    }
}

// A database written before unattributed blocks stored an empty miner carries the
// literal "Unknown"; blockInit normalises it, so the placeholder is applied at
// render rather than baked into the data.
func TestOpenDBMigratesUnknownMiner(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "watches.db")
    if err := openDB(path); err != nil { t.Fatalf("openDB: %v", err) }
    if err := flushBlocks([]*blockInfo{
        {Height: 800001, Hash: "h1", Miner: "Unknown"},
        {Height: 800002, Hash: "h2", Miner: "AntPool"},
    }); err != nil {
        t.Fatalf("store blocks: %v", err)
    }
    closeDB()
    if err := openDB(path); err != nil { t.Fatalf("reopen: %v", err) }
    defer closeDB()
    var bi, ok = loadBlock(800001)
    if !ok || bi.Miner != "" {
        t.Errorf("legacy Unknown miner = %q, want empty after the migration", bi.Miner)
    }
    if bi, ok = loadBlock(800002); !ok || bi.Miner != "AntPool" {
        t.Errorf("an attributed miner must survive the migration, got %q", bi.Miner)
    }
}

// The Mini App's block list windows the blocks bucket by height rather than
// by an offset, which is what keeps a batch stable while the chain grows at the
// head. The cursor arithmetic is where that can go wrong, so this drives it
// against a real database.
//
// the newest batch, newest first, with more below it
//
// the next batch continues strictly below it — no row is repeated, and none
// is skipped
//
// a height the bucket does not hold still starts below it
//
// what a new block prepends: everything above the height the list topped out
// at, and nothing that is already on screen
//
// nothing new is a legitimate answer, and the sentinel keeps its height
//
// more new blocks than a batch holds is what tells the app to replace the
// whole list rather than prepend a piece of it
//
// Back restores everything from the tip down to the block that was opened
func TestAppBlocksWindows(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    for h := int64(700000); h < 700050; h++ {
        if err := flushBlocks([]*blockInfo{{Height: h, Hash: "h", NumTx: 3, Miner: "PoolX"}}); err != nil {
            t.Fatalf("store %d: %v", h, err)
        }
    }
    var heights = func(b app.Blocks) []int64 {
        var out []int64
        for _, r := range b.Rows { out = append(out, r.Num) }
        return out
    }
    var top = appSource{}.Blocks("", app.Range{})
    if !top.OK || len(top.Rows) != blocksPerPage || !top.More {
        t.Fatalf("newest batch: %d rows more=%v", len(top.Rows), top.More)
    }
    if top.Top != 700049 || top.Next != 700038 || heights(top)[0] != 700049 {
        t.Fatalf("newest batch spans %d..%d, want 700049..700038", top.Top, top.Next)
    }
    var next = appSource{}.Blocks("", app.Range{Before: top.Next})
    if len(next.Rows) != blocksPerPage || next.Top != 700037 || next.Next != 700026 {
        t.Fatalf("second batch spans %d..%d, want 700037..700026", next.Top, next.Next)
    }
    if b := (appSource{}).Blocks("", app.Range{Before: 800000}); b.Top != 700049 {
        t.Fatalf("a before above the tip should start at the tip, got %d", b.Top)
    }
    if b := (appSource{}).Blocks("", app.Range{Before: 700000}); b.OK || len(b.Rows) != 0 {
        t.Fatalf("nothing is below the oldest block, got %d rows", len(b.Rows))
    }
    var fresh = appSource{}.Blocks("", app.Range{After: 700046})
    if got := heights(fresh); len(got) != 3 || got[0] != 700049 || got[2] != 700047 {
        t.Fatalf("prepend = %v, want 700049..700047", got)
    }
    if fresh.More {
        t.Error("three new blocks fit in a batch; nothing was cut off")
    }
    var none = appSource{}.Blocks("", app.Range{After: 700049})
    if len(none.Rows) != 0 || none.Top != 700049 {
        t.Fatalf("nothing new = %d rows top=%d, want 0 rows at 700049", len(none.Rows), none.Top)
    }
    if b := (appSource{}).Blocks("", app.Range{After: 700000}); !b.More {
        t.Error("49 new blocks do not fit in one batch; More must say so")
    }
    var back = appSource{}.Blocks("", app.Range{Down: 700030})
    if got := heights(back); len(got) != 20 || got[0] != 700049 || got[19] != 700030 {
        t.Fatalf("restored %d rows spanning %v, want 700049..700030", len(got), got)
    }
    if !back.More {
        t.Error("there are older blocks below the restored list, so it keeps its sentinel")
    }
    if b := (appSource{}).Blocks("", app.Range{Down: 700000}); b.More {
        t.Error("a list restored to the oldest block has nothing more to append")
    }
}
