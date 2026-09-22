package addrindex

import "bytes"
import "encoding/hex"
import "errors"
import "fmt"
import "path/filepath"
import "strconv"
import "testing"
import "database/sql"
import _ "modernc.org/sqlite"
import "bitnsbot/core/coretest"
import "bitnsbot/cursors"

// The two tables as openDB creates them.
const ddl = `create table addrindex (shard INTEGER PRIMARY KEY, data BLOB NOT NULL);
    create table cursors (name TEXT PRIMARY KEY, place INTEGER NOT NULL)`

func openTestDB(t *testing.T) {
    t.Helper()
    var handle, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "addrindex.sqlite"))
    if err != nil { t.Fatalf("open: %v", err) }
    if _, err := handle.Exec(ddl); err != nil { t.Fatal(err) }
    if err := cursors.Init(handle); err != nil { t.Fatalf("cursors: %v", err) }
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() {
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

// LookupLast is the newest-first counterpart of Lookup, and the paging the
// address page's transaction views rely on: it returns the most recent touches
// oldest-first among themselves, and capped tells whether older ones remain.
func TestLookupLast(t *testing.T) {
    openTestDB(t)
    var script = []byte("0014lastpages")
    var prefix = string(Prefix(script))
    for h := uint32(0); h < 12; h++ {
        if err := merge(map[string][]Touch{prefix: {{Height: h, TxIndex: 1}}}, int(h)); err != nil {
            t.Fatalf("merge at height %d: %v", h, err)
        }
    }
    var got, capped = LookupLast(script, 5)
    if !capped { t.Fatal("five of twelve: older touches remain, so capped should be true") }
    if len(got) != 5 { t.Fatalf("touches = %d, want 5", len(got)) }
    for i, want := range []uint32{7, 8, 9, 10, 11} {
        if got[i].Height != want {
            t.Fatalf("touches = %v, want heights 7..11", got)
        }
    }
    var all, allCapped = LookupLast(script, 100)
    if allCapped || len(all) != 12 {
        t.Fatalf("full history = %d touches (capped=%v), want all 12", len(all), allCapped)
    }
    if all[0].Height != 0 || all[11].Height != 11 {
        t.Fatalf("full history order = %v, want 0..11", all)
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

func TestCursor(t *testing.T) {
    openTestDB(t)
    if _, ok := Cursor(); ok {
        t.Fatal("expected no cursor on a fresh index")
    }
    if err := merge(nil, 500); err != nil {
        t.Fatalf("merge: %v", err)
    }
    var h, ok = Cursor()
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

// chain is a node holding blocks, which BlockAt reads over RPC the way it reads a
// real one. Every getblock is recorded in fetched, and a height in err answers
// with the node's error instead.
type chain struct {
    tip     int
    blocks  map[int]Block
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
func (c *chain) respond(method string, params []interface{}) (interface{}, error) {
    switch method {
    case "getblockcount":
        return c.tip, nil
    case "getblockhash":
        return fmt.Sprint(params[0]), nil
    case "getblock":
        var height, _ = strconv.Atoi(params[0].(string))
        c.fetched = append(c.fetched, height)
        if c.err[height] { return nil, errors.New("fetch failed") }
        var amount = func(p Payment) map[string]interface{} {
            return map[string]interface{}{"value": float64(p.Sat) / 1e8, "scriptPubKey": map[string]string{"hex": hex.EncodeToString(p.Script)}}
        }
        var txs = []interface{}{}
        for _, tx := range c.blocks[height].Txs {
            var vin = []interface{}{}
            var vout = []interface{}{}
            for _, p := range tx.Spent { vin = append(vin, map[string]interface{}{"prevout": amount(p)}) }
            for _, p := range tx.Outputs { vout = append(vout, amount(p)) }
            txs = append(txs, map[string]interface{}{"vin": vin, "vout": vout})
        }
        return map[string]interface{}{"time": c.blocks[height].Time, "tx": txs}, nil
    }
    return nil, fmt.Errorf("unexpected call %s %v", method, params)
}

// A tiny synthetic chain: height 0 pays scriptA, height 1 spends it (pays
// scriptB), height 2 is unrelated. Each block is one transaction, built from the
// scripts as hex.
func syntheticBlock(t *testing.T, outScripts []string, spentScripts []string) Block {
    var tx Tx
    for _, side := range []struct {
        scripts []string
        into    *[]Payment
    }{{outScripts, &tx.Outputs}, {spentScripts, &tx.Spent}} {
        for _, s := range side.scripts {
            var raw, err = hex.DecodeString(s)
            if err != nil { t.Fatalf("bad script hex: %v", err) }
            *side.into = append(*side.into, Payment{Script: raw})
        }
    }
    return Block{Hash: "synthetic", Txs: []Tx{tx}}
}

// a second pass with nothing new fetches nothing
func TestCatchUp(t *testing.T) {
    openTestDB(t)
    var saved = chunkSize
    t.Cleanup(func() { chunkSize = saved })
    chunkSize = 1000
    var scriptA, scriptB = "0014aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0014bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    var src = serve(t, &chain{tip: 2, blocks: map[int]Block{
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
    var height, ok = Cursor()
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
    var blocks = map[int]Block{}
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
    var height, _ = Cursor()
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
