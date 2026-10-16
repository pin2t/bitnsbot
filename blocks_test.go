package main

import "context"
import "encoding/json"
import "fmt"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "strings"
import "testing"
import "bitnsbot/core/coretest"
import "bitnsbot/blocks"
import "bitnsbot/app"
import "bitnsbot/cursors"

func TestFormatBlock(t *testing.T) {
    var bi = &blocks.Info{
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

// A block the collector has not reached is computed for /info and for the Mini
// App's block page, and shown, but not stored: the highest height stored is where
// the collector resumes, so a lookup above it would step the collector over every
// block in between.
func TestBlockLookupDoesNotStore(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var srv = coretest.Server(t, func(method string, params []any) (any, error) {
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
    useTelegram(t, tg.URL)
    block(context.Background(), "tg", 1, 500)
    if len(sent) != 1 || !strings.Contains(sent[0], "Block #500") {
        t.Fatalf("/info 500 replied %q", sent)
    }
    if info := appBlockInfo("", 500); !info.OK {
        t.Fatal("the Mini App's block page did not show a block it had to compute")
    }
    if _, ok := blocks.Load(500); ok {
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
    if _, err := db.Exec("insert into cursors (name, place) values ('miners', 7)"); err != nil { t.Fatal(err) }
    closeDB()
    if err := openDB(path); err != nil { t.Fatalf("reopen: %v", err) }
    defer closeDB()
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
        if err := blocks.Flush([]*blocks.Info{{Height: h, Hash: "h", NumTx: 3, Miner: "PoolX"}}); err != nil {
            t.Fatalf("store %d: %v", h, err)
        }
    }
    var heights = func(b app.Blocks) []int64 {
        var out []int64
        for _, r := range b.Rows { out = append(out, r.Num) }
        return out
    }
    var top = appBlocks("", app.Range{})
    if !top.OK || len(top.Rows) != blocksPerPage || !top.More {
        t.Fatalf("newest batch: %d rows more=%v", len(top.Rows), top.More)
    }
    if top.Top != 700049 || top.Next != 700038 || heights(top)[0] != 700049 {
        t.Fatalf("newest batch spans %d..%d, want 700049..700038", top.Top, top.Next)
    }
    var next = appBlocks("", app.Range{Before: top.Next})
    if len(next.Rows) != blocksPerPage || next.Top != 700037 || next.Next != 700026 {
        t.Fatalf("second batch spans %d..%d, want 700037..700026", next.Top, next.Next)
    }
    if b := appBlocks("", app.Range{Before: 800000}); b.Top != 700049 {
        t.Fatalf("a before above the tip should start at the tip, got %d", b.Top)
    }
    if b := appBlocks("", app.Range{Before: 700000}); b.OK || len(b.Rows) != 0 {
        t.Fatalf("nothing is below the oldest block, got %d rows", len(b.Rows))
    }
    var fresh = appBlocks("", app.Range{After: 700046})
    if got := heights(fresh); len(got) != 3 || got[0] != 700049 || got[2] != 700047 {
        t.Fatalf("prepend = %v, want 700049..700047", got)
    }
    if fresh.More {
        t.Error("three new blocks fit in a batch; nothing was cut off")
    }
    var none = appBlocks("", app.Range{After: 700049})
    if len(none.Rows) != 0 || none.Top != 700049 {
        t.Fatalf("nothing new = %d rows top=%d, want 0 rows at 700049", len(none.Rows), none.Top)
    }
    if b := appBlocks("", app.Range{After: 700000}); !b.More {
        t.Error("49 new blocks do not fit in one batch; More must say so")
    }
    var back = appBlocks("", app.Range{Down: 700030})
    if got := heights(back); len(got) != 20 || got[0] != 700049 || got[19] != 700030 {
        t.Fatalf("restored %d rows spanning %v, want 700049..700030", len(got), got)
    }
    if !back.More {
        t.Error("there are older blocks below the restored list, so it keeps its sentinel")
    }
    if b := appBlocks("", app.Range{Down: 700000}); b.More {
        t.Error("a list restored to the oldest block has nothing more to append")
    }
}
