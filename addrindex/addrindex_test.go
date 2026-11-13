package addrindex

import "bytes"
import "context"
import "encoding/hex"
import "errors"
import "fmt"
import "path/filepath"
import "strconv"
import "testing"
import "database/sql"
import _ "modernc.org/sqlite"
import "bitnsbot/core"
import "bitnsbot/core/coretest"
import "bitnsbot/cursors"

func openTestDB(t *testing.T) {
    t.Helper()
    var handle, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "addrindex.sqlite"))
    if err != nil { t.Fatalf("open: %v", err) }
    if err := cursors.Init(handle); err != nil { t.Fatalf("cursors: %v", err) }
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    var saved = recent
    recent = 0
    t.Cleanup(func() {
        recent = saved
        handle.Close()
        db = nil
        cursors.Init(nil)
    })
}

// an address with no touches returns nothing, not an error
func TestMergeAndLookup(t *testing.T) {
    openTestDB(t)
    var script = []byte("0014deadbeef")
    var prefix = string(Prefix(script))
    if err := merge(map[string][]Touch{prefix: {{Height: 10, TxIndex: 0}}}, 10); err != nil {
        t.Fatalf("merge: %v", err)
    }
    if err := merge(map[string][]Touch{prefix: {{Height: 12, TxIndex: 3}}}, 12); err != nil {
        t.Fatalf("merge: %v", err)
    }
    var got, capped = Lookup(script, 10000)
    var want = []Touch{{Height: 10, TxIndex: 0}, {Height: 12, TxIndex: 3}}
    if capped { t.Fatal("unexpectedly capped") }
    if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
        t.Fatalf("touches = %v, want %v", got, want)
    }
    var empty, _ = Lookup([]byte("nevertouched"), 10000)
    if len(empty) != 0 {
        t.Fatalf("expected no touches, got %v", empty)
    }
}

// The cap is now a *read* limit, not a write limit: everything is stored, and
// only the lookup stops early. This is the visible payoff of the sharded,
// append-only layout — the previous key-per-address scheme had to cap what it
// wrote, because appending rewrote the address's whole value every time.
//
// raising the limit must reveal the rest: nothing was ever dropped on disk
func TestLookupCaps(t *testing.T) {
    openTestDB(t)
    var script = []byte("hotaddress")
    var prefix = string(Prefix(script))
    for h := uint32(0); h < 5; h++ {
        if err := merge(map[string][]Touch{prefix: {{Height: h, TxIndex: 0}}}, int(h)); err != nil {
            t.Fatalf("merge at height %d: %v", h, err)
        }
    }
    var got, capped = Lookup(script, 3)
    if !capped {
        t.Fatal("expected capped once the read hit limit 3")
    }
    if len(got) != 3 {
        t.Fatalf("touches = %d, want exactly 3", len(got))
    }
    if got[0].Height != 0 || got[2].Height != 2 {
        t.Fatalf("expected the oldest 3 touches, got %v", got)
    }
    var all, stillCapped = Lookup(script, 100)
    if stillCapped || len(all) != 5 {
        t.Fatalf("full history = %d touches (capped=%v), want all 5 stored", len(all), stillCapped)
    }
}

// LookupFrom is the newest-first counterpart of Lookup, and the paging the
// address page's transaction views rely on: it returns the most recent touches
// newest first without reversing the whole list, and capped tells whether
// older ones remain. The height bound is where the scan starts from — the
// caller's way to skip the ranges above an address's last activity.
func TestLookupFrom(t *testing.T) {
    openTestDB(t)
    var script = []byte("0014lastpages")
    var prefix = string(Prefix(script))
    for h := uint32(0); h < 12; h++ {
        if err := merge(map[string][]Touch{prefix: {{Height: h, TxIndex: 1}}}, int(h)); err != nil {
            t.Fatalf("merge at height %d: %v", h, err)
        }
    }
    var got, capped = LookupFrom(script, 0, 5)
    if !capped { t.Fatal("five of twelve: older touches remain, so capped should be true") }
    if len(got) != 5 { t.Fatalf("touches = %d, want 5", len(got)) }
    for i, want := range []uint32{11, 10, 9, 8, 7} {
        if got[i].Height != want {
            t.Fatalf("touches = %v, want heights 11..7 newest first", got)
        }
    }
    var bounded, boundedCapped = LookupFrom(script, 8, 5)
    if !boundedCapped || len(bounded) != 5 {
        t.Fatalf("history at and below 8 = %d touches (capped=%v), want the newest 5 of 0..8 with more behind", len(bounded), boundedCapped)
    }
    for i, want := range []uint32{8, 7, 6, 5, 4} {
        if bounded[i].Height != want {
            t.Fatalf("bounded touches = %v, want heights 8..4 newest first", bounded)
        }
    }
    var all, allCapped = LookupFrom(script, 0, 100)
    if allCapped || len(all) != 12 {
        t.Fatalf("full history = %d touches (capped=%v), want all 12", len(all), allCapped)
    }
    if all[0].Height != 11 || all[11].Height != 0 {
        t.Fatalf("full history order = %v, want 11..0", all)
    }
}

// A height bound must reach back into the ranges below the one holding it.
// The address page always passes one — the block of the address's last
// activity — and a scan that read the bound's range and the ones above it
// returned only the touches sharing that last 1000-block range.
func TestLookupFromReadsOlderRanges(t *testing.T) {
    openTestDB(t)
    var script = []byte("0014manyranges")
    var prefix = string(Prefix(script))
    for _, h := range []uint32{10, 1010, 2010, 2020, 3010} {
        if err := merge(map[string][]Touch{prefix: {{Height: h, TxIndex: 2}}}, int(h)); err != nil {
            t.Fatalf("merge at height %d: %v", h, err)
        }
    }
    var got, capped = LookupFrom(script, 2500, 10)
    if capped { t.Fatal("all four touches at or below 2500 fit, so capped should be false") }
    var want = []uint32{2020, 2010, 1010, 10}
    if len(got) != len(want) { t.Fatalf("touches = %v, want heights %v", got, want) }
    for i, h := range want {
        if got[i].Height != h { t.Fatalf("touches = %v, want heights %v", got, want) }
    }
    var page, pageCapped = LookupFrom(script, 3010, 2)
    if !pageCapped || len(page) != 2 || page[0].Height != 3010 || page[1].Height != 2020 {
        t.Fatalf("first page from 3010 = %v (capped=%v), want 3010, 2020 with more behind", page, pageCapped)
    }
}
func TestSharedShardIsolation(t *testing.T) {
    openTestDB(t)
    var a, b []byte
    for i := 0; i < 1000000 && b == nil; i++ {
        var candidate = []byte(fmt.Sprintf("script-%d", i))
        if a == nil {
            a = candidate
            continue
        }
        var pa, pc = Prefix(a), Prefix(candidate)
        if bytes.Equal(pa[:shardLen], pc[:shardLen]) && !bytes.Equal(pa, pc) {
            b = candidate
        }
    }
    if b == nil { t.Fatal("could not find two scripts sharing a shard") }
    t.Logf("shard %x shared by %q and %q", Prefix(a)[:shardLen], a, b)
    merge(map[string][]Touch{string(Prefix(a)): {{Height: 10, TxIndex: 1}}}, 10)
    merge(map[string][]Touch{string(Prefix(b)): {{Height: 20, TxIndex: 2}}}, 20)
    var ta, _ = Lookup(a, 10000)
    if len(ta) != 1 || ta[0].Height != 10 || ta[0].TxIndex != 1 {
        t.Fatalf("script A got %v, want only its own touch at height 10", ta)
    }
    var tb, _ = Lookup(b, 10000)
    if len(tb) != 1 || tb[0].Height != 20 || tb[0].TxIndex != 2 {
        t.Fatalf("script B got %v, want only its own touch at height 20", tb)
    }
}

// Touches must come back in chronological order across range boundaries, since
// each range is a separate key and the reply lists history oldest-first.
func TestLookupSpansRanges(t *testing.T) {
    openTestDB(t)
    var script = []byte("spansranges")
    var prefix = string(Prefix(script))
    var heights = []uint32{5, rangeBlocks + 7, 3*rangeBlocks + 1}
    for _, h := range heights {
        merge(map[string][]Touch{prefix: {{Height: h, TxIndex: 0}}}, int(h))
    }
    var got, _ = Lookup(script, 10000)
    if len(got) != len(heights) {
        t.Fatalf("touches = %d, want %d across %d ranges", len(got), len(heights), len(heights))
    }
    for i, h := range heights {
        if got[i].Height != h {
            t.Fatalf("touch %d height = %d, want %d (order must be chronological)", i, got[i].Height, h)
        }
    }
}

func TestHeight(t *testing.T) {
    openTestDB(t)
    if _, ok := Height(); ok {
        t.Fatal("expected no cursor on a fresh index")
    }
    if err := merge(nil, 500); err != nil {
        t.Fatalf("merge: %v", err)
    }
    var h, ok = Height()
    if !ok || h != 500 {
        t.Fatalf("cursor = %+v ok=%v, want Height=500", h, ok)
    }
}

// Two addresses whose scripts happen to share an 8-byte SHA-256 prefix must not
// corrupt each other's history — Prefix truncates, so this is the one place a
// collision could silently merge two addresses' touches into one bucket entry.
// This doesn't (and can't, at 8 bytes) test a *real* collision; it documents the
// contract Lookup's caller relies on: two different scripts get different keys
// unless a genuine prefix collision occurs, which callers must post-filter for.
func TestDistinctScriptsDistinctKeys(t *testing.T) {
    openTestDB(t)
    var a, b = []byte("scriptA"), []byte("scriptB")
    if string(Prefix(a)) == string(Prefix(b)) {
        t.Fatal("test scripts collide by chance, pick different ones")
    }
    merge(map[string][]Touch{string(Prefix(a)): {{Height: 1, TxIndex: 0}}}, 1)
    merge(map[string][]Touch{string(Prefix(b)): {{Height: 2, TxIndex: 0}}}, 2)
    var ta, _ = Lookup(a, 10000)
    var tb, _ = Lookup(b, 10000)
    if len(ta) != 1 || ta[0].Height != 1 {
        t.Fatalf("scriptA touches = %v", ta)
    }
    if len(tb) != 1 || tb[0].Height != 2 {
        t.Fatalf("scriptB touches = %v", tb)
    }
}

// chain is a node holding blocks, which core.BlockAt reads over RPC the way it reads a
// real one. Every getblock is recorded in fetched, and a height in err answers
// with the node's error instead.
type chain struct {
    tip     int
    blocks  map[int]core.Block
    fetched []int
    err     map[int]bool
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
    case "getblockheader":
        var height, _ = strconv.Atoi(params[0].(string))
        return map[string]any{"hash": params[0], "height": height}, nil
    case "getblock":
        var height, _ = strconv.Atoi(params[0].(string))
        c.fetched = append(c.fetched, height)
        if c.err[height] { return nil, errors.New("fetch failed") }
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

// A tiny synthetic chain: height 0 pays scriptA, height 1 spends it (pays
// scriptB), height 2 is unrelated. Each block is one transaction, built from the
// scripts as hex.
func syntheticBlock(t *testing.T, outScripts []string, spentScripts []string) core.Block {
    var tx core.Tx
    for _, side := range []struct {
        scripts []string
        into    *[]core.Payment
    }{{outScripts, &tx.Outputs}, {spentScripts, &tx.Spent}} {
        for _, s := range side.scripts {
            var raw, err = hex.DecodeString(s)
            if err != nil { t.Fatalf("bad script hex: %v", err) }
            *side.into = append(*side.into, core.Payment{Script: raw})
        }
    }
    return core.Block{Hash: "synthetic", Txs: []core.Tx{tx}}
}

// a second pass with nothing new fetches nothing
func TestCatchUp(t *testing.T) {
    openTestDB(t)
    var saved = chunkSize
    t.Cleanup(func() { chunkSize = saved })
    chunkSize = 1000
    var scriptA, scriptB = "0014aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0014bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    var src = serve(t, &chain{tip: 2, blocks: map[int]core.Block{
        0: syntheticBlock(t, []string{scriptA}, nil),
        1: syntheticBlock(t, []string{scriptB}, []string{scriptA}),
        2: syntheticBlock(t, nil, nil),
    }})
    if err := Build(); err != nil { t.Fatalf("catchUp: %v", err) }
    var rawA, _ = hex.DecodeString(scriptA)
    var rawB, _ = hex.DecodeString(scriptB)
    var touchesA, _ = Lookup(rawA, 10000)
    var touchesB, _ = Lookup(rawB, 10000)
    if len(touchesA) != 2 || touchesA[0].Height != 0 || touchesA[1].Height != 1 {
        t.Fatalf("scriptA touches = %v, want heights [0, 1]", touchesA)
    }
    if len(touchesB) != 1 || touchesB[0].Height != 1 {
        t.Fatalf("scriptB touches = %v, want [height 1]", touchesB)
    }
    var height, ok = Height()
    if !ok || height != 2 {
        t.Fatalf("cursor = %+v ok=%v, want Height=2", height, ok)
    }
    src.fetched = nil
    if err := Build(); err != nil { t.Fatalf("second catchUp: %v", err) }
    if len(src.fetched) != 0 {
        t.Fatalf("second catchUp refetched %v, want nothing (already at tip)", src.fetched)
    }
}

// A chunk boundary must flush before the next chunk starts, and a failed fetch
// must abandon the chunk without advancing the cursor — same reasoning as the
// miners collector: stepping the cursor over an unfetched block would drop it
// from the index permanently.
//
// heights 0-1 (one full chunk) must have been flushed before the failure at 3
//
// fixing the block and retrying picks up from where it stopped
func TestCatchUpChunksAndRetries(t *testing.T) {
    openTestDB(t)
    var savedChunk = chunkSize
    t.Cleanup(func() { chunkSize = savedChunk })
    chunkSize = 2
    var script = "0014cccccccccccccccccccccccccccccccccccccccc"
    var blocks = map[int]core.Block{}
    for h := 0; h < 5; h++ {
        blocks[h] = syntheticBlock(t, []string{script}, nil)
    }
    var src = serve(t, &chain{tip: 4, blocks: blocks, err: map[int]bool{3: true}})
    if err := Build(); err == nil {
        t.Fatal("expected an error from the failing block")
    }
    var raw, _ = hex.DecodeString(script)
    var touches, _ = Lookup(raw, 10000)
    if len(touches) != 2 {
        t.Fatalf("touches after partial catch-up = %d, want 2 (the first chunk only)", len(touches))
    }
    var height, _ = Height()
    if height != 1 {
        t.Fatalf("cursor = %d, want 1 (stuck before the failed block)", height)
    }
    src.err = nil
    src.fetched = nil
    if err := Build(); err != nil { t.Fatalf("retry: %v", err) }
    var deepFetched = append([]int{}, src.fetched...)
    if len(deepFetched) != 3 || deepFetched[0] != 2 {
        t.Fatalf("retry fetched %v, want [2 3 4]", deepFetched)
    }
    touches, _ = Lookup(raw, 10000)
    if len(touches) != 5 {
        t.Fatalf("touches after retry = %d, want 5", len(touches))
    }
}

// The key is the on-disk format, not a detail a lookup could hide: the 2-byte
// shard above the 4-byte range, read as one integer — the six bytes the bbolt key
// held, big-endian. A packing that merge and Lookup changed together would pass
// every other test here and leave every index already built unreadable.
func TestKeyLayout(t *testing.T) {
    var prefix = []byte{0xab, 0xcd, 1, 2, 3, 4, 5, 6}
    if got := key(prefix, 300); got != 0xabcd0000012c {
        t.Errorf("key = %x, want abcd0000012c", got)
    }
    if got := key(prefix, 0xffffffff); got >= key([]byte{0xab, 0xce, 0, 0, 0, 0, 0, 0}, 0) {
        t.Errorf("the shard's last range %x does not sort below the next shard", got)
    }
}

// Writing to an index nobody opened is an error, never a silent success — that
// is how a missing Init once went unnoticed through a whole chain backfill.
func TestMergeWithoutInit(t *testing.T) {
    if err := merge(map[string][]Touch{"p": {{Height: 1}}}, 1); err == nil {
        t.Error("merge with no database reported success")
    }
}

// A pass merges every block below the tip less `recent` into addrindex and
// leaves the newest `recent` staged in recaddrindex, a row per (shard, block)
// plus the marker; a lookup reads both, in order, either way round. A later
// pass folds what the new tip has pushed below the line, with no block read
// twice.
func TestRecentBlocksStayStaged(t *testing.T) {
    openTestDB(t)
    recent = 2
    var script = "0014dddddddddddddddddddddddddddddddddddddddd"
    var blocks = map[int]core.Block{}
    for h := 0; h < 8; h++ { blocks[h] = syntheticBlock(t, []string{script}, nil) }
    var src = serve(t, &chain{tip: 5, blocks: blocks})
    if err := Build(); err != nil { t.Fatalf("build: %v", err) }
    if at, _ := cursors.Get(cursors.AddrIndex); at != 3 { t.Fatalf("cursor = %d, want 3", at) }
    if h, ok := Height(); !ok || h != 5 { t.Fatalf("height = %d, %v; want 5", h, ok) }
    var staged []int64
    var rows, err = db.Query("select distinct block from recaddrindex order by block")
    if err != nil { t.Fatal(err) }
    for rows.Next() {
        var b int64
        rows.Scan(&b)
        staged = append(staged, b)
    }
    rows.Close()
    if len(staged) != 2 || staged[0] != 4 || staged[1] != 5 { t.Fatalf("staged blocks = %v, want [4 5]", staged) }
    var raw = mustHex(t, script)
    var got, _ = Lookup(raw, 100)
    if len(got) != 6 { t.Fatalf("touches = %v, want heights 0..5", got) }
    for i, tch := range got {
        if tch.Height != uint32(i) { t.Fatalf("touches = %v, want heights 0..5 oldest first", got) }
    }
    var newest, capped = LookupFrom(raw, 0, 3)
    if !capped || len(newest) != 3 || newest[0].Height != 5 || newest[2].Height != 3 {
        t.Fatalf("newest = %v (capped=%v), want 5, 4, 3 with more behind", newest, capped)
    }
    var bounded, _ = LookupFrom(raw, 4, 2)
    if len(bounded) != 2 || bounded[0].Height != 4 || bounded[1].Height != 3 {
        t.Fatalf("bounded = %v, want 4, 3", bounded)
    }
    src.tip = 7
    src.fetched = nil
    if err := Build(); err != nil { t.Fatalf("second build: %v", err) }
    if len(src.fetched) != 2 || src.fetched[0] != 6 || src.fetched[1] != 7 {
        t.Fatalf("second build fetched %v, want [6 7]", src.fetched)
    }
    if at, _ := cursors.Get(cursors.AddrIndex); at != 5 { t.Fatalf("cursor = %d, want 5", at) }
    got, _ = Lookup(raw, 100)
    if len(got) != 8 { t.Fatalf("touches = %v, want heights 0..7", got) }
    for i, tch := range got {
        if tch.Height != uint32(i) { t.Fatalf("touches = %v, want heights 0..7 oldest first", got) }
    }
}

// Rolling a staged block back deletes it and every block above it, so the next
// pass reads them from the replacing chain; a block already merged into
// addrindex is left alone.
func TestRollbackRereadsDisconnectedBlocks(t *testing.T) {
    openTestDB(t)
    recent = 3
    var old, replaced = "0014eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "0014ffffffffffffffffffffffffffffffffffffffff"
    var blocks = map[int]core.Block{}
    for h := 0; h < 6; h++ { blocks[h] = syntheticBlock(t, []string{old}, nil) }
    var src = serve(t, &chain{tip: 5, blocks: blocks})
    if err := Build(); err != nil { t.Fatalf("build: %v", err) }
    Rollback("1")
    if got, _ := Lookup(mustHex(t, old), 100); len(got) != 6 {
        t.Fatalf("rolling back a merged block removed touches: %v", got)
    }
    Rollback("4")
    if h, _ := Height(); h != 3 { t.Fatalf("height after rollback = %d, want 3", h) }
    if got, _ := Lookup(mustHex(t, old), 100); len(got) != 4 || got[3].Height != 3 {
        t.Fatalf("touches after rollback = %v, want heights 0..3", got)
    }
    src.blocks[4] = syntheticBlock(t, []string{replaced}, nil)
    src.blocks[5] = syntheticBlock(t, []string{replaced}, nil)
    src.fetched = nil
    if err := Build(); err != nil { t.Fatalf("rebuild: %v", err) }
    if len(src.fetched) != 2 || src.fetched[0] != 4 || src.fetched[1] != 5 {
        t.Fatalf("rebuild fetched %v, want [4 5]", src.fetched)
    }
    if got, _ := Lookup(mustHex(t, replaced), 100); len(got) != 2 || got[0].Height != 4 {
        t.Fatalf("replacing chain touches = %v, want heights 4, 5", got)
    }
    if got, _ := Lookup(mustHex(t, old), 100); len(got) != 4 {
        t.Fatalf("old chain touches = %v, want only heights 0..3", got)
    }
}

// A block the cursor has passed is never staged: its touches are in addrindex
// already, and a staged copy would be read twice by every lookup.
func TestRecordSkipsMergedBlocks(t *testing.T) {
    openTestDB(t)
    var script = "0014abababababababababababababababababababab"
    serve(t, &chain{tip: 2, blocks: map[int]core.Block{2: syntheticBlock(t, []string{script}, nil)}})
    if err := merge(nil, 2); err != nil { t.Fatal(err) }
    if err := record(context.Background(), 2); err != nil { t.Fatal(err) }
    if have, _ := holds(2); have { t.Fatal("a block at the cursor was staged") }
}
