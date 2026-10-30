package addrstat

import "bytes"
import "encoding/hex"
import "fmt"
import "path/filepath"
import "encoding/binary"
import "errors"
import "strconv"
import "sync"
import "testing"
import "database/sql"
import _ "modernc.org/sqlite"
import "bitnsbot/addrindex"
import "bitnsbot/core"
import "bitnsbot/core/coretest"
import "bitnsbot/cursors"
import "bitnsbot/signals"

// Two addresses the tests follow and one they do not, each built from a real
// script so the address and the script meet on the key the scan matches by.
var scriptA = append([]byte{0x00, 20}, bytes.Repeat([]byte{0xa1}, 20)...)
var scriptB = append([]byte{0x76, 0xa9, 20}, append(bytes.Repeat([]byte{0xb2}, 20), 0x88, 0xac)...)
var scriptC = append([]byte{0x51, 32}, bytes.Repeat([]byte{0xc3}, 32)...)

var addrA = addrindex.Address(scriptA)
var addrB = addrindex.Address(scriptB)
var addrC = addrindex.Address(scriptC)

// tx is one transaction's two sides: what it pays, and what it spends.
type tx struct {
    outs  []core.Payment
    spent []core.Payment
}

func pay(script []byte, sat int64) core.Payment { return core.Payment{Script: script, Sat: sat} }

// blockOf is a block mined at when, holding txs in order.
func blockOf(when int64, txs ...tx) core.Block {
    var blk = core.Block{Hash: "fixture", Time: when}
    for _, t := range txs {
        blk.Txs = append(blk.Txs, core.Tx{Outputs: t.outs, Spent: t.spent})
    }
    return blk
}

// chain is a node holding blocks, which the scan reads over RPC the way it reads
// a real one. Every getblock is recorded in fetched, under mu, since a chunk's
// blocks are read on several goroutines at once; a height in fail answers with
// an error, and onFetch, when set, is called with each height asked for.
type chain struct {
    tip     int
    blocks  map[int]core.Block
    fetched []int
    fail    map[int]bool
    onFetch func(int)
    mu      sync.Mutex
}

// serve points core at the chain for the rest of the test.
func serve(t *testing.T, c *chain) *chain {
    coretest.Start(t, c.respond)
    return c
}

// respond names each block by its height, and answers getblock with the block
// as verbosity 3 reports it: an input per spent prevout, and none carrying one
// for a transaction that spends nothing, which is how a coinbase reads.
func (c *chain) respond(method string, params []any) (any, error) {
    switch method {
    case "getblockcount":
        return c.tip, nil
    case "getblockhash":
        return fmt.Sprint(params[0]), nil
    case "getblock":
        var height, _ = strconv.Atoi(params[0].(string))
        c.mu.Lock()
        c.fetched = append(c.fetched, height)
        var failed = c.fail[height]
        c.mu.Unlock()
        if c.onFetch != nil { c.onFetch(height) }
        if failed { return nil, errors.New("block unavailable") }
        var amount = func(p core.Payment) map[string]any {
            return map[string]any{"value": float64(p.Sat) / 1e8, "scriptPubKey": map[string]string{"hex": hex.EncodeToString(p.Script)}}
        }
        var txs = []any{}
        for _, tx := range c.blocks[height].Txs {
            var vin = []any{}
            var vout = []any{}
            for _, p := range tx.Spent { vin = append(vin, map[string]any{"prevout": amount(p)}) }
            for _, p := range tx.Outputs { vout = append(vout, amount(p)) }
            txs = append(txs, map[string]any{"vin": vin, "vout": vout})
        }
        return map[string]any{"time": c.blocks[height].Time, "tx": txs}, nil
    }
    return nil, fmt.Errorf("unexpected call %s %v", method, params)
}

// open creates the tables the scan writes into and points the package at them.
// There is nothing to seed: a row is an address the scan has found holding
// coins, so the table starts empty whatever the chain holds.
func open(t *testing.T) *sql.DB {
    t.Helper()
    var handle, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
    if err != nil { t.Fatalf("open: %v", err) }
    if err := cursors.Init(handle); err != nil { t.Fatalf("cursors: %v", err) }
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { handle.Close(); db = nil })
    return handle
}

func statOf(t *testing.T, addr string) Stat {
    t.Helper()
    var s Stat
    var err = db.QueryRow(`select type, balance, recv, sent, flow, fees, txs, first, last
        from addrstat where addr = ?`, addr).Scan(&s.Type, &s.Balance, &s.Recv, &s.Sent, &s.Flow,
        &s.Fees, &s.Txs, &s.First, &s.Last)
    if err != nil { t.Fatalf("no row for %s: %v", addr, err) }
    return s
}

// rows is how many rows the table holds under addr — 0 or 1, since the address
// is the primary key.
func rows(t *testing.T, addr string) int {
    t.Helper()
    var n int
    if err := db.QueryRow("select count(*) from addrstat where addr = ?", addr).Scan(&n); err != nil {
        t.Fatal(err)
    }
    return n
}

// The chain pays addrA twice and then spends from it, paying part to addrB and
// part back to addrA and leaving the rest as a fee. Everything the records hold
// is asserted against what those blocks actually moved — and every one of the
// three addresses is a row the scan wrote itself, nothing having been seeded.
//
// spends both of addrA's outputs, pays 3000 to addrB and 4000 back, so the fee
// is 1000
//
// the fee is charged once, though the transaction spends two of its outputs
//
// three transactions: two paying it, one both spending from it and paying it
func TestCollectGathersStatistics(t *testing.T) {
    open(t)
    serve(t, &chain{tip: 2, blocks: map[int]core.Block{
        0: blockOf(1000, tx{outs: []core.Payment{pay(scriptA, 5000)}}),
        1: blockOf(2000, tx{outs: []core.Payment{pay(scriptA, 3000), pay(scriptC, 100)}}),
        2: blockOf(3000,
            tx{outs: []core.Payment{pay(scriptC, 50)}},
            tx{outs: []core.Payment{pay(scriptB, 3000), pay(scriptA, 4000)},
                spent: []core.Payment{pay(scriptA, 5000), pay(scriptA, 3000)}}),
    }})
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    var a = statOf(t, addrA)
    if a.Recv != 12000 || a.Sent != 8000 || a.Balance != 4000 || a.Flow != 20000 {
        t.Errorf("addrA amounts = %+v", a)
    }
    if a.Fees != 1000 { t.Errorf("addrA fees = %d, want 1000", a.Fees) }
    if a.Txs != 3 { t.Errorf("addrA txs = %d, want 3", a.Txs) }
    if a.First != 1000 || a.Last != 3000 { t.Errorf("addrA dates = %d..%d, want 1000..3000", a.First, a.Last) }
    if a.Type != "segwit" { t.Errorf("addrA type = %q, want segwit", a.Type) }
    var b = statOf(t, addrB)
    if b.Recv != 3000 || b.Sent != 0 || b.Balance != 3000 || b.Txs != 1 || b.Fees != 0 {
        t.Errorf("addrB = %+v", b)
    }
    if b.Type != "p2pkh" { t.Errorf("addrB type = %q, want p2pkh", b.Type) }
    var c = statOf(t, addrC)
    if c.Recv != 150 || c.Balance != 150 || c.Txs != 2 { t.Errorf("addrC = %+v", c) }
    if c.Type != "taproot" { t.Errorf("addrC type = %q, want taproot", c.Type) }
    if Count() != 3 { t.Errorf("Count = %d, want the three addresses the chain paid", Count()) }
}

// An address the chunk emptied loses its row: the table is the addresses that
// hold coins, so what a spent-out change address leaves behind is nothing.
func TestEmptiedAddressIsRemoved(t *testing.T) {
    open(t)
    serve(t, &chain{tip: 1, blocks: map[int]core.Block{
        0: blockOf(1000, tx{outs: []core.Payment{pay(scriptA, 5000)}}),
        1: blockOf(2000, tx{outs: []core.Payment{pay(scriptB, 4000)}, spent: []core.Payment{pay(scriptA, 5000)}}),
    }})
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    if rows(t, addrA) != 0 { t.Errorf("addrA kept a row holding nothing: %+v", statOf(t, addrA)) }
    if b := statOf(t, addrB); b.Balance != 4000 { t.Errorf("addrB = %+v", b) }
}

// An address emptied by a later chunk loses the row an earlier one gave it, so
// the delete has to reach a row the chunk did not insert.
func TestEmptiedAcrossChunksIsRemoved(t *testing.T) {
    open(t)
    var src = serve(t, &chain{tip: 0, blocks: map[int]core.Block{
        0: blockOf(1000, tx{outs: []core.Payment{pay(scriptA, 5000)}}),
        1: blockOf(2000, tx{outs: []core.Payment{pay(scriptB, 4000)}, spent: []core.Payment{pay(scriptA, 5000)}}),
    }})
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    if a := statOf(t, addrA); a.Balance != 5000 { t.Errorf("addrA = %+v", a) }
    src.tip = 1
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    if rows(t, addrA) != 0 { t.Error("addrA kept a row after a later chunk emptied it") }
}

// A chunk that reaches flushKeys addresses merges what it has and takes the
// cursor to the block it had read, so a scan of the modern chain is bounded by
// the addresses it holds rather than by its thousand blocks.
func TestChunkFlushesOnKeys(t *testing.T) {
    open(t)
    var keys, size = flushKeys, chunkSize
    flushKeys, chunkSize = 1, 1000
    defer func() { flushKeys, chunkSize = keys, size }()
    serve(t, &chain{tip: 2, blocks: map[int]core.Block{
        0: blockOf(1000, tx{outs: []core.Payment{pay(scriptA, 5000)}}),
        1: blockOf(2000, tx{outs: []core.Payment{pay(scriptB, 3000)}}),
        2: blockOf(3000, tx{outs: []core.Payment{pay(scriptC, 2000)}}),
    }})
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    if h, ok := cursors.Get(cursors.AddrStat); !ok || h != 2 { t.Errorf("cursor = %d ok=%v, want 2", h, ok) }
    if Count() != 3 { t.Errorf("Count = %d, want 3", Count()) }
}

// A transaction that both pays an address and spends from it is one transaction
// for it, not two.
func TestTransactionCountedOncePerAddress(t *testing.T) {
    open(t)
    serve(t, &chain{tip: 0, blocks: map[int]core.Block{
        0: blockOf(1000, tx{
            outs:  []core.Payment{pay(scriptA, 400), pay(scriptA, 100)},
            spent: []core.Payment{pay(scriptA, 900)},
        }),
    }})
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    var a = statOf(t, addrA)
    if a.Txs != 1 { t.Errorf("txs = %d, want 1", a.Txs) }
    if a.Recv != 500 || a.Sent != 900 || a.Balance != -400 { t.Errorf("%+v", a) }
    if a.Fees != 400 { t.Errorf("fees = %d, want 400", a.Fees) }
}

// The scan resumes from its cursor: a second pass fetches only what is new and
// adds to what is stored rather than counting a block twice.
func TestCollectResumes(t *testing.T) {
    open(t)
    var src = serve(t, &chain{tip: 0, blocks: map[int]core.Block{
        0: blockOf(1000, tx{outs: []core.Payment{pay(scriptA, 500)}}),
        1: blockOf(2000, tx{outs: []core.Payment{pay(scriptA, 700)}}),
    }})
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    src.tip, src.fetched = 1, nil
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    if len(src.fetched) != 1 || src.fetched[0] != 1 {
        t.Errorf("fetched %v on the second pass, want only block 1", src.fetched)
    }
    var a = statOf(t, addrA)
    if a.Recv != 1200 || a.Txs != 2 { t.Errorf("%+v, want 1200 over 2 transactions", a) }
    if a.First != 1000 || a.Last != 2000 { t.Errorf("dates = %d..%d", a.First, a.Last) }
    if h, ok := cursors.Get(cursors.AddrStat); !ok || h != 1 { t.Errorf("cursor = %d ok=%v, want 1", h, ok) }
}

// The three ranked address lists are built from these records, so a pass that
// moved them says so — and one that found nothing to do says nothing, or every
// idle tick would rebuild three indexes for no reason.
//
// nothing new: the cursor is at the tip, so there is nothing to announce
func TestCollectSignalsWhenItHasCaughtUp(t *testing.T) {
    open(t)
    signals.Reset()
    var wake = signals.Subscribe(signals.AddrStat)
    serve(t, &chain{tip: 0, blocks: map[int]core.Block{
        0: blockOf(1000, tx{outs: []core.Payment{pay(scriptA, 500)}}),
    }})
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    select {
    case <-wake:
    default:
        t.Error("a pass that scanned a block did not signal")
    }
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    select {
    case <-wake:
        t.Error("an idle pass signalled a rebuild")
    default:
    }
}

// An address the scan has not been past has no row, so there is nothing for Get
// to answer with and the live path answers it instead.
func TestGetWaitsForSomethingToReport(t *testing.T) {
    open(t)
    if _, ok := Get(addrA); ok { t.Fatal("answered before the scan had been past the address") }
    serve(t, &chain{tip: 0, blocks: map[int]core.Block{
        0: blockOf(1000, tx{outs: []core.Payment{pay(scriptA, 500)}}),
    }})
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    var s, ok = Get(addrA)
    if !ok || s.Recv != 500 { t.Fatalf("Get = %+v ok=%v", s, ok) }
    if _, ok := Get(addrC); ok { t.Error("answered for an address the chain never paid") }
}

// Init is run on every start, so one must leave the scan's place alone.
func TestRepeatedInitKeepsTheCursor(t *testing.T) {
    var handle = open(t)
    serve(t, &chain{tip: 0, blocks: map[int]core.Block{
        0: blockOf(1000, tx{outs: []core.Payment{pay(scriptA, 500)}}),
    }})
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    for i := 0; i < 3; i++ {
        if err := Init(handle); err != nil { t.Fatalf("init %d: %v", i, err) }
    }
    if h, ok := cursors.Get(cursors.AddrStat); !ok || h != 0 { t.Errorf("cursor = %d ok=%v, want 0", h, ok) }
    if a := statOf(t, addrA); a.Recv != 500 { t.Errorf("a repeated Init changed the record: %+v", a) }
}

// scriptOf is a distinct segwit script for each n, so a test can pay as many
// addresses as it likes.
func scriptOf(n int) []byte {
    var hash = make([]byte, 20)
    binary.BigEndian.PutUint32(hash, uint32(n)+1)
    return append([]byte{0x00, 20}, hash...)
}

// busyChain is blocks 0..tip, each paying a few addresses old and new and
// spending what an earlier block paid, so addresses run across blocks — paid in
// one, spent in another, some emptied — which is what a merge of per-block
// records has to get right.
func busyChain(tip int) *chain {
    var c = &chain{tip: tip, blocks: map[int]core.Block{}}
    for h := 0; h <= tip; h++ {
        var outs = []core.Payment{pay(scriptOf(h), 1000), pay(scriptOf(h%7), 300), pay(scriptOf(h+1000), 50)}
        var t = tx{outs: outs}
        if h > 0 { t.spent = []core.Payment{pay(scriptOf(h-1), 1000)} }
        c.blocks[h] = blockOf(int64(10_000-h*3+(h%5)*7), t)
    }
    return c
}

// table is every row, keyed by address, for comparing two scans' results.
func table(t *testing.T) map[string]Stat {
    t.Helper()
    var out = map[string]Stat{}
    var rs, err = db.Query(`select addr, type, balance, recv, sent, flow, fees, txs, first, last from addrstat`)
    if err != nil { t.Fatal(err) }
    defer rs.Close()
    for rs.Next() {
        var addr string
        var s Stat
        if err := rs.Scan(&addr, &s.Type, &s.Balance, &s.Recv, &s.Sent, &s.Flow, &s.Fees, &s.Txs, &s.First, &s.Last); err != nil {
            t.Fatal(err)
        }
        out[addr] = s
    }
    return out
}

// Reading a chunk's blocks on several goroutines writes exactly the table one
// goroutine does, chunks and mid-chunk merges included.
func TestParallelMatchesSequential(t *testing.T) {
    var saved, size, keys = workers, chunkSize, flushKeys
    defer func() { workers, chunkSize, flushKeys = saved, size, keys }()
    chunkSize, flushKeys = 40, 25
    var results []map[string]Stat
    for _, n := range []int{1, 8} {
        workers = n
        open(t)
        serve(t, busyChain(150))
        if err := Collect(); err != nil { t.Fatalf("Collect with %d workers: %v", n, err) }
        if h, ok := cursors.Get(cursors.AddrStat); !ok || h != 150 { t.Errorf("%d workers: cursor = %d ok=%v", n, h, ok) }
        results = append(results, table(t))
    }
    if len(results[0]) == 0 { t.Fatal("the sequential scan wrote nothing") }
    if len(results[0]) != len(results[1]) { t.Fatalf("%d rows sequentially, %d in parallel", len(results[0]), len(results[1])) }
    for addr, want := range results[0] {
        if got := results[1][addr]; got != want { t.Errorf("%s: parallel %+v, sequential %+v", addr, got, want) }
    }
}

// A merge taken on flushKeys covers a run of blocks with nothing missing, so the
// cursor never passes a block that has not been applied: no block is asked for
// at or below the cursor, and none is asked for twice.
func TestParallelFlushLeavesNoGap(t *testing.T) {
    var saved, keys = workers, flushKeys
    defer func() { workers, flushKeys = saved, keys }()
    workers, flushKeys = 8, 3
    open(t)
    var c = busyChain(120)
    c.onFetch = func(h int) {
        if at, ok := cursors.Get(cursors.AddrStat); ok && int64(h) <= at { t.Errorf("block %d asked for with the cursor at %d", h, at) }
    }
    serve(t, c)
    if err := Collect(); err != nil { t.Fatalf("Collect: %v", err) }
    var seen = map[int]int{}
    for _, h := range c.fetched { seen[h]++ }
    for h := 0; h <= 120; h++ {
        if seen[h] != 1 { t.Errorf("block %d fetched %d times", h, seen[h]) }
    }
}

// A block that cannot be read fails the pass and merges nothing of the chunk it
// is in, so the next pass reads the same heights again.
func TestParallelFailureMergesNothing(t *testing.T) {
    var saved = workers
    defer func() { workers = saved }()
    workers = 4
    open(t)
    var c = busyChain(30)
    c.fail = map[int]bool{17: true}
    serve(t, c)
    if err := Collect(); err == nil { t.Fatal("a pass with an unreadable block succeeded") }
    if _, ok := cursors.Get(cursors.AddrStat); ok { t.Error("the cursor moved past a failed chunk") }
    if Count() != 0 { t.Errorf("Count = %d after a failed chunk, want 0", Count()) }
    c.mu.Lock()
    c.fail = nil
    c.mu.Unlock()
    if err := Collect(); err != nil { t.Fatalf("retry: %v", err) }
    if h, ok := cursors.Get(cursors.AddrStat); !ok || h != 30 { t.Errorf("cursor = %d ok=%v after the retry", h, ok) }
}
