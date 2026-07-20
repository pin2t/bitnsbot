package addrindex

import "bytes"
import "context"
import "encoding/hex"
import "errors"
import "path/filepath"
import "testing"

import "go.etcd.io/bbolt"

func openTestDB(t *testing.T) {
    var d, err = bbolt.Open(filepath.Join(t.TempDir(), "addrindex.db"), 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    if err := Init(d); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { d.Close(); db = nil })
}

func TestMergeAndLookup(t *testing.T) {
    openTestDB(t)
    var script = []byte("0014deadbeef")
    var prefix = string(Prefix(script))
    if err := merge(map[string][]Touch{prefix: {{Height: 10, TxIndex: 0}}}, Cursor{Height: 10}); err != nil {
        t.Fatalf("merge: %v", err)
    }
    if err := merge(map[string][]Touch{prefix: {{Height: 12, TxIndex: 3}}}, Cursor{Height: 12}); err != nil {
        t.Fatalf("merge: %v", err)
    }
    var got, capped = Lookup(script)
    var want = []Touch{{Height: 10, TxIndex: 0}, {Height: 12, TxIndex: 3}}
    if capped { t.Fatal("unexpectedly capped") }
    if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
        t.Fatalf("touches = %v, want %v", got, want)
    }
    // an address with no touches returns nothing, not an error
    var empty, _ = Lookup([]byte("nevertouched"))
    if len(empty) != 0 {
        t.Fatalf("expected no touches, got %v", empty)
    }
}

func TestLookupCaps(t *testing.T) {
    openTestDB(t)
    var saved = maxTouches
    t.Cleanup(func() { maxTouches = saved })
    maxTouches = 3
    var script = []byte("hotaddress")
    var prefix = string(Prefix(script))
    for h := uint32(0); h < 5; h++ {
        if err := merge(map[string][]Touch{prefix: {{Height: h, TxIndex: 0}}}, Cursor{Height: int64(h)}); err != nil {
            t.Fatalf("merge at height %d: %v", h, err)
        }
    }
    var got, capped = Lookup(script)
    if !capped {
        t.Fatal("expected capped after exceeding maxTouches")
    }
    if len(got) != 3 {
        t.Fatalf("touches = %d, want exactly maxTouches (3)", len(got))
    }
    if got[0].Height != 0 || got[2].Height != 2 {
        t.Fatalf("expected the OLDEST 3 touches kept (0,1,2), got %v", got)
    }
}

func TestCursor(t *testing.T) {
    openTestDB(t)
    if _, ok := LoadCursor(); ok {
        t.Fatal("expected no cursor on a fresh index")
    }
    if err := merge(nil, Cursor{Height: 500}); err != nil {
        t.Fatalf("merge: %v", err)
    }
    var c, ok = LoadCursor()
    if !ok || c.Height != 500 {
        t.Fatalf("cursor = %+v ok=%v, want Height=500", c, ok)
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
    merge(map[string][]Touch{string(Prefix(a)): {{Height: 1, TxIndex: 0}}}, Cursor{Height: 1})
    merge(map[string][]Touch{string(Prefix(b)): {{Height: 2, TxIndex: 0}}}, Cursor{Height: 2})
    var ta, _ = Lookup(a)
    var tb, _ = Lookup(b)
    if len(ta) != 1 || ta[0].Height != 1 {
        t.Fatalf("scriptA touches = %v", ta)
    }
    if len(tb) != 1 || tb[0].Height != 2 {
        t.Fatalf("scriptB touches = %v", tb)
    }
}

type fakeSource struct {
    tip     int64
    blocks  map[int64]Block
    fetched []int64
    err     map[int64]bool
}

func (f *fakeSource) Tip(ctx context.Context) (int64, error) { return f.tip, nil }

func (f *fakeSource) BlockAt(ctx context.Context, height int64) (Block, error) {
    f.fetched = append(f.fetched, height)
    if f.err[height] { return Block{}, errors.New("fetch failed") }
    return f.blocks[height], nil
}

// A tiny synthetic chain: height 0 pays scriptA, height 1 spends it (pays
// scriptB), height 2 is unrelated. Built directly from parsed shapes via a
// Block whose Raw/Spent are pre-serialized isn't needed here since catchUp calls
// indexBlock, which calls the real parsers — so these use minimal-but-valid wire
// bytes: one coinbase-shaped input (0x00 right after version is unambiguously
// the segwit marker on the real wire, precisely because a legacy transaction can
// never have zero inputs — a fake "0 inputs" tx would be misparsed as segwit).
func syntheticBlock(t *testing.T, outScripts []string, spentScripts []string) Block {
    var out = new(bytes.Buffer)
    out.Write(make([]byte, 80)) // header, unread by the parser
    out.WriteByte(1)            // tx count = 1
    out.Write(make([]byte, 4))  // version
    out.WriteByte(1)            // input count = 1 (a coinbase-shaped input)
    out.Write(make([]byte, 36)) // null prevout hash + index
    out.WriteByte(0)            // empty scriptSig
    out.Write(make([]byte, 4))  // sequence
    out.WriteByte(byte(len(outScripts)))
    for _, s := range outScripts {
        var raw, err = hex.DecodeString(s)
        if err != nil { t.Fatalf("bad script hex: %v", err) }
        out.Write(make([]byte, 8)) // value
        out.WriteByte(byte(len(raw)))
        out.Write(raw)
    }
    out.Write(make([]byte, 4)) // locktime

    var spent = new(bytes.Buffer)
    spent.WriteByte(1) // tx count = 1
    spent.WriteByte(byte(len(spentScripts)))
    for _, s := range spentScripts {
        var raw, err = hex.DecodeString(s)
        if err != nil { t.Fatalf("bad script hex: %v", err) }
        spent.Write(make([]byte, 8))
        spent.WriteByte(byte(len(raw)))
        spent.Write(raw)
    }
    return Block{Hash: "synthetic", Raw: out.Bytes(), Spent: spent.Bytes()}
}

func TestCatchUp(t *testing.T) {
    openTestDB(t)
    var saved = chunkSize
    t.Cleanup(func() { chunkSize = saved })
    chunkSize = 1000
    var scriptA, scriptB = "0014aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0014bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    var src = &fakeSource{tip: 2, blocks: map[int64]Block{
        0: syntheticBlock(t, []string{scriptA}, nil),
        1: syntheticBlock(t, []string{scriptB}, []string{scriptA}),
        2: syntheticBlock(t, nil, nil),
    }}
    if err := catchUp(src); err != nil { t.Fatalf("catchUp: %v", err) }
    var rawA, _ = hex.DecodeString(scriptA)
    var rawB, _ = hex.DecodeString(scriptB)
    var touchesA, _ = Lookup(rawA)
    var touchesB, _ = Lookup(rawB)
    if len(touchesA) != 2 || touchesA[0].Height != 0 || touchesA[1].Height != 1 {
        t.Fatalf("scriptA touches = %v, want heights [0, 1]", touchesA)
    }
    if len(touchesB) != 1 || touchesB[0].Height != 1 {
        t.Fatalf("scriptB touches = %v, want [height 1]", touchesB)
    }
    var cursor, ok = LoadCursor()
    if !ok || cursor.Height != 2 {
        t.Fatalf("cursor = %+v ok=%v, want Height=2", cursor, ok)
    }
    // a second pass with nothing new fetches nothing
    src.fetched = nil
    if err := catchUp(src); err != nil { t.Fatalf("second catchUp: %v", err) }
    if len(src.fetched) != 0 {
        t.Fatalf("second catchUp refetched %v, want nothing (already at tip)", src.fetched)
    }
}

// A chunk boundary must flush before the next chunk starts, and a failed fetch
// must abandon the chunk without advancing the cursor — same reasoning as the
// miners collector: stepping the cursor over an unfetched block would drop it
// from the index permanently.
func TestCatchUpChunksAndRetries(t *testing.T) {
    openTestDB(t)
    var savedChunk = chunkSize
    t.Cleanup(func() { chunkSize = savedChunk })
    chunkSize = 2
    var script = "0014cccccccccccccccccccccccccccccccccccccccc"
    var blocks = map[int64]Block{}
    for h := int64(0); h < 5; h++ {
        blocks[h] = syntheticBlock(t, []string{script}, nil)
    }
    var src = &fakeSource{tip: 4, blocks: blocks, err: map[int64]bool{3: true}}
    if err := catchUp(src); err == nil {
        t.Fatal("expected an error from the failing block")
    }
    // heights 0-1 (one full chunk) must have been flushed before the failure at 3
    var raw, _ = hex.DecodeString(script)
    var touches, _ = Lookup(raw)
    if len(touches) != 2 {
        t.Fatalf("touches after partial catch-up = %d, want 2 (the first chunk only)", len(touches))
    }
    var cursor, _ = LoadCursor()
    if cursor.Height != 1 {
        t.Fatalf("cursor = %d, want 1 (stuck before the failed block)", cursor.Height)
    }
    // fixing the block and retrying picks up from where it stopped
    src.err = nil
    src.fetched = nil
    if err := catchUp(src); err != nil { t.Fatalf("retry: %v", err) }
    var deepFetched = append([]int64{}, src.fetched...)
    if len(deepFetched) != 3 || deepFetched[0] != 2 {
        t.Fatalf("retry fetched %v, want [2 3 4]", deepFetched)
    }
    touches, _ = Lookup(raw)
    if len(touches) != 5 {
        t.Fatalf("touches after retry = %d, want 5", len(touches))
    }
}
