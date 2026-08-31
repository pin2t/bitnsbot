package main

import "encoding/binary"
import "flag"
import "fmt"
import "encoding/hex"
import "encoding/json"
import "io"
import "net/http"
import "net/http/httptest"
import "os"
import "path/filepath"
import "sort"
import "strconv"
import "strings"
import "testing"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"

// The script the fixture's address is paid to. Its bytes are all that matter —
// the index is keyed by scriptPubKey, and no address format is ever decoded.
var payScript = mustHex("76a914000102030405060708090a0b0c0d0e0f1011121388ac")
var otherScript = mustHex("76a914aabbccddeeff00112233445566778899aabbccdd88ac")

// address is what the list tests hand to the node to validate; the node decides
// what script it maps to, so its text is arbitrary. The addresses actbuild
// records are not — those are encoded from the scripts themselves, so the tests
// ask scriptAddress rather than naming them.
const address = "37QAiiRLSHEsMPu3SXT9AKWDoZsZxtfuRP"

func mustHex(s string) []byte {
    var b, err = hex.DecodeString(s)
    if err != nil { panic(err) }
    return b
}

func varint(n int) []byte {
    if n < 0xfd { return []byte{byte(n)} }
    var b = []byte{0xfd, 0, 0}
    binary.LittleEndian.PutUint16(b[1:], uint16(n))
    return b
}

// serialTx builds a non-segwit transaction with inputs inputs and one output
// per script in pays. The input scripts are not written here: the spending side
// of a touch comes from the separate spent-outputs blob, not from the block.
//
// inputs is never 0 — a zero input count is the segwit marker, so even a
// coinbase carries one input, as it does on the real chain.
func serialTx(inputs int, pays [][]byte) []byte {
    var out []byte
    out = append(out, 1, 0, 0, 0) // version
    out = append(out, varint(inputs)...)
    for i := 0; i < inputs; i++ {
        out = append(out, make([]byte, 36)...) // prevout hash + index
        out = append(out, varint(0)...)        // empty scriptSig
        out = append(out, 0xff, 0xff, 0xff, 0xff)
    }
    out = append(out, varint(len(pays))...)
    for _, s := range pays {
        out = append(out, make([]byte, 8)...) // value, unread by the indexer
        out = append(out, varint(len(s))...)
        out = append(out, s...)
    }
    out = append(out, 0, 0, 0, 0) // locktime
    return out
}

// serialBlock and serialSpent are the two REST payloads for one block. They must
// describe the same transactions in the same order — that alignment is what lets
// a touch be keyed by position instead of by txid.
func serialBlock(txs [][]byte) []byte {
    var out = make([]byte, 80) // header
    out = append(out, varint(len(txs))...)
    for _, t := range txs { out = append(out, t...) }
    return out
}

func serialSpent(perTx [][][]byte) []byte {
    var out = varint(len(perTx))
    for _, scripts := range perTx {
        out = append(out, varint(len(scripts))...)
        for _, s := range scripts {
            out = append(out, make([]byte, 8)...)
            out = append(out, varint(len(s))...)
            out = append(out, s...)
        }
    }
    return out
}

// chainBlocks is the fixture, indexed by height from genesis. Block 1 pays the
// address, block 2 spends from it and pays change back; 0 and 3 are unrelated.
func chainBlocks() [][2][]byte {
    var coinbase = serialTx(1, [][]byte{otherScript})
    var plain = [2][]byte{serialBlock([][]byte{coinbase}), serialSpent([][][]byte{{}})}
    return [][2][]byte{
        plain,
        {serialBlock([][]byte{coinbase, serialTx(1, [][]byte{payScript})}),
            serialSpent([][][]byte{{}, {otherScript}})},
        {serialBlock([][]byte{coinbase, serialTx(1, [][]byte{otherScript, payScript})}),
            serialSpent([][][]byte{{}, {payScript}})},
        plain,
    }
}

// txidAt is the id the fake node reports for a given (height, position).
func txidAt(height uint32, index int) string {
    return strings.Repeat(string(rune('a'+height)), 63) + string(rune('0'+index))
}

// fakeCore serves both halves of what the tool needs: Core's REST interface for
// the build, and its JSON-RPC for the lookups.
func fakeCore(t *testing.T, tip int) *httptest.Server {
    var blocks = chainBlocks()
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var p = r.URL.Path
        switch {
        case p == "/rest/chaininfo.json":
            json.NewEncoder(w).Encode(map[string]int{"blocks": tip})
        case strings.HasPrefix(p, "/rest/blockhashbyheight/"):
            var h, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, "/rest/blockhashbyheight/"), ".bin"))
            // the client reverses these bytes to render the hash, so the height
            // goes last here to come out first in the hex
            var raw = make([]byte, 32)
            raw[31] = byte(h)
            w.Write(raw)
        case strings.HasPrefix(p, "/rest/block/"):
            w.Write(blocks[heightOfHash(strings.TrimSuffix(strings.TrimPrefix(p, "/rest/block/"), ".bin"))][0])
        case strings.HasPrefix(p, "/rest/spenttxouts/"):
            w.Write(blocks[heightOfHash(strings.TrimSuffix(strings.TrimPrefix(p, "/rest/spenttxouts/"), ".bin"))][1])
        case p == "/":
            rpcReply(t, w, r)
        default:
            t.Errorf("unexpected request %s", p)
            w.WriteHeader(404)
        }
    }))
    t.Cleanup(srv.Close)
    return srv
}

// hashOfHeight and heightOfHash are the fixture's stand-in for real hashes: the
// height lives in the leading byte, which is where the REST client's reversal
// puts it.
func hashOfHeight(height int) string {
    var raw = make([]byte, 32)
    raw[0] = byte(height)
    return hex.EncodeToString(raw)
}

func heightOfHash(hash string) int {
    var b, _ = hex.DecodeString(hash[:2])
    return int(b[0])
}

// rpcCall is one request, whether it arrived alone or inside a batch. The id is
// left untyped on purpose: a single call carries a string one and a batch
// carries numbers, and decoding into an int would fail on the former.
type rpcCall struct {
    ID     interface{}   `json:"id"`
    Method string        `json:"method"`
    Params []interface{} `json:"params"`
}

// batchID is the numeric id a batch entry carries, which is what the reply must
// be matched back by.
func batchID(v interface{}) int {
    if n, ok := v.(float64); ok { return int(n) }
    return -1
}

// requests counts every request the fake has served, so a test can assert that
// a pass which should read only files touched the node not at all.
var requests int

func rpcReply(t *testing.T, w http.ResponseWriter, r *http.Request) {
    requests++
    var body, rerr = io.ReadAll(r.Body)
    if rerr != nil {
        t.Errorf("read rpc: %v", rerr)
        return
    }
    // Core answers a batch — an array of requests — with an array of results.
    if len(body) > 0 && body[0] == '[' {
        var reqs []rpcCall
        if err := json.Unmarshal(body, &reqs); err != nil {
            t.Errorf("decode batch: %v", err)
            return
        }
        var out []map[string]interface{}
        for _, req := range reqs {
            out = append(out, map[string]interface{}{"id": batchID(req.ID), "result": answer(t, req)})
        }
        json.NewEncoder(w).Encode(out)
        return
    }
    var req rpcCall
    if err := json.Unmarshal(body, &req); err != nil {
        t.Errorf("decode rpc: %v", err)
        return
    }
    json.NewEncoder(w).Encode(map[string]interface{}{"result": answer(t, req)})
}

func answer(t *testing.T, req rpcCall) interface{} {
    var out interface{}
    var reply = func(v interface{}) { out = v }
    switch req.Method {
    case "validateaddress":
        reply(map[string]interface{}{"isvalid": true, "scriptPubKey": hex.EncodeToString(payScript)})
    case "getblockhash":
        reply(hashOfHeight(int(req.Params[0].(float64))))
    case "getblock":
        var height = uint32(heightOfHash(req.Params[0].(string)))
        var ids = []string{txidAt(height, 0), txidAt(height, 1)}
        reply(map[string]interface{}{"tx": ids})
    case "getrawtransaction":
        reply(txDetail(req.Params[0].(string)))
    default:
        t.Errorf("unexpected rpc method %s", req.Method)
    }
    return out
}

// txDetail: block 1's transaction pays the address 20000 sat; block 2's spends
// that and pays 10000 back as change, so the net is -10000.
func txDetail(txid string) map[string]interface{} {
    var vout = func(addr string, btc float64) map[string]interface{} {
        return map[string]interface{}{"value": btc, "scriptPubKey": map[string]string{"address": addr}}
    }
    switch txid {
    case txidAt(1, 1):
        return map[string]interface{}{"txid": txid, "time": 1634850000,
            "vin":  []interface{}{map[string]interface{}{"prevout": vout("someone-else", 0.0003)}},
            "vout": []interface{}{vout(address, 0.0002)}}
    case txidAt(2, 1):
        return map[string]interface{}{"txid": txid, "time": 1667300400,
            "vin":  []interface{}{map[string]interface{}{"prevout": vout(address, 0.0002)}},
            "vout": []interface{}{vout("someone-else", 0.0001), vout(address, 0.0001)}}
    }
    return map[string]interface{}{"txid": txid, "time": 0}
}

func openIndex(t *testing.T) {
    var handle, err = bbolt.Open(filepath.Join(t.TempDir(), "ai.db"), 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    db = handle
    t.Cleanup(func() { handle.Close(); db = nil })
    if err := addrindex.Init(handle); err != nil { t.Fatalf("init: %v", err) }
}

// activeAddresses reads back what actbuild recorded.
func activeAddresses(t *testing.T) map[string]int {
    var out = map[string]int{}
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(activeBucket)
        if b == nil { return nil }
        return b.ForEach(func(k, v []byte) error {
            out[string(k)] = int(binary.BigEndian.Uint64(v))
            return nil
        })
    })
    return out
}

func capture(t *testing.T, f func()) string {
    var old = os.Stdout
    var r, w, err = os.Pipe()
    if err != nil { t.Fatalf("pipe: %v", err) }
    os.Stdout = w
    f()
    w.Close()
    os.Stdout = old
    var out, _ = io.ReadAll(r)
    return string(out)
}

// The whole tool end to end: build the index from a fake node's REST interface,
// then list an address out of it over the fake's JSON-RPC.
func TestBuildThenList(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000}

    var built = capture(t, func() { build(opt) })
    if !strings.Contains(built, "Building blocks 0..3") {
        t.Errorf("build did not report its range: %q", built)
    }
    if at, ok := addrindex.Cursor(); !ok || at != 3 {
        t.Fatalf("cursor = %d, %v; want 3 — the build must advance the shared cursor", at, ok)
    }

    var out = capture(t, func() { list(opt, address) })
    var lines = strings.Split(strings.TrimSpace(out), "\n")
    if len(lines) != 3 {
        t.Fatalf("want two transactions and a summary, got:\n%s", out)
    }
    // the funding transaction, then the spend that paid change back
    if want := "21 oct 2021 21:00 bbbbbbbb..bbbbbbb1    20000 sat"; lines[0] != want {
        t.Errorf("line 1 = %q, want %q", lines[0], want)
    }
    if want := "1 nov 2022 11:00  cccccccc..ccccccc1   -10000 sat"; lines[1] != want {
        t.Errorf("line 2 = %q, want %q", lines[1], want)
    }
    var wantSummary = "Summary: Balance 10000 sats, Received 30000 sats, Sent 20000 sats, " +
        "Transactions 2, Activity: from 21 oct 2021 till 1 nov 2022"
    if lines[2] != wantSummary {
        t.Errorf("summary = %q,\n    want %q", lines[2], wantSummary)
    }
}

// A second build with nothing new must not redo work — the cursor is the whole
// point of sharing the format with the bot.
func TestBuildResumesFromCursor(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000}
    capture(t, func() { build(opt) })
    var again = capture(t, func() { build(opt) })
    if !strings.Contains(again, "already at the tip") {
        t.Errorf("a second build should be a no-op, got %q", again)
    }
}

// An address the index has never seen says so rather than printing an empty
// summary that reads like a real answer.
func TestListUnknownAddress(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var out = capture(t, func() { list(&options{url: srv.URL, limit: 1000}, address) })
    if !strings.Contains(out, "No transactions in the index") {
        t.Errorf("an unbuilt index should say so, got %q", out)
    }
}

func TestFormatting(t *testing.T) {
    if got := stamp(1634850000); got != "21 oct 2021 21:00" {
        t.Errorf("stamp = %q", got)
    }
    if got := day(1667300400); got != "1 nov 2022" {
        t.Errorf("day = %q — the activity range carries no clock time", got)
    }
    if got := shortID(strings.Repeat("a", 64)); got != "aaaaaaaa..aaaaaaaa" {
        t.Errorf("shortID = %q", got)
    }
    if got := shortID("short"); got != "short" {
        t.Errorf("shortID mangled a short id: %q", got)
    }
    // a spend is a negative line, so a column of them sums to the balance
    if got := amount(-10000); got != "-10000 sat" {
        t.Errorf("amount = %q", got)
    }
}

func TestSummaryAndNet(t *testing.T) {
    var s summary
    s.add(entry{at: 1634850000, received: 20000})
    s.add(entry{at: 1667300400, received: 10000, sent: 20000})
    if got := s.String(); !strings.Contains(got, "Balance 10000 sats") ||
        !strings.Contains(got, "Received 30000 sats") || !strings.Contains(got, "Sent 20000 sats") {
        t.Errorf("summary = %q", got)
    }
    if got := (entry{received: 10000, sent: 20000}).net(); got != -10000 {
        t.Errorf("net = %d, want -10000", got)
    }
    // an empty history has no activity range to report
    if got := (summary{}).String(); strings.Contains(got, "Activity") {
        t.Errorf("summary = %q; nothing happened, so there is no range", got)
    }
}

func TestTook(t *testing.T) {
    for _, c := range []struct {
        d    time.Duration
        want string
    }{
        {42 * time.Second, "42 sec"},
        {90 * time.Second, "1 min 30 sec"},
        {2*time.Hour + 5*time.Minute, "2 h 5 min"},
    } {
        if got := took(c.d); got != c.want {
            t.Errorf("took(%s) = %q, want %q", c.d, got, c.want)
        }
    }
}

// writeBlockFiles lays out a Core blocks directory: one blk file per group of
// blocks, framed and obfuscated exactly as Core writes them, plus the xor.dat
// that holds the key.
func writeBlockFiles(t *testing.T, groups [][][]byte) string {
    var dir = t.TempDir()
    var key = []byte{0x66, 0xcb, 0x13, 0xcf, 0x57, 0x2a, 0x2e, 0x5f}
    if err := os.WriteFile(filepath.Join(dir, "xor.dat"), key, 0600); err != nil {
        t.Fatalf("xor.dat: %v", err)
    }
    for n, blocks := range groups {
        var raw []byte
        for _, b := range blocks {
            var size = make([]byte, 4)
            binary.LittleEndian.PutUint32(size, uint32(len(b)))
            raw = append(raw, magic...)
            raw = append(raw, size...)
            raw = append(raw, b...)
        }
        // Core preallocates, so the written records are followed by zeros — the
        // reader has to stop there rather than read them as a record
        raw = append(raw, make([]byte, 64)...)
        for i := range raw { raw[i] ^= key[i%len(key)] }
        var name = filepath.Join(dir, fmt.Sprintf("blk%05d.dat", n))
        if err := os.WriteFile(name, raw, 0600); err != nil { t.Fatalf("write %s: %v", name, err) }
    }
    return dir
}

// chainFiles is the fixture as Core would store it: the same blocks the index is
// built from, two per file.
func chainFiles(t *testing.T) string {
    var blocks = chainBlocks()
    return writeBlockFiles(t, [][][]byte{
        {blocks[0][0], blocks[1][0]},
        {blocks[2][0], blocks[3][0]},
    })
}

// The reader has to undo Core's obfuscation, follow the magic-and-length
// framing, and stop at the padding rather than read it as a record.
func TestBlockReaderWalksAFile(t *testing.T) {
    var blocks = chainBlocks()
    var dir = writeBlockFiles(t, [][][]byte{{blocks[0][0], blocks[1][0], blocks[2][0]}})
    var key, err = xorKey(dir)
    if err != nil { t.Fatalf("key: %v", err) }
    var names, ferr = blockFiles(dir)
    if ferr != nil { t.Fatalf("files: %v", ferr) }
    if len(names) != 1 { t.Fatalf("found %d files, want 1", len(names)) }
    var r, oerr = openBlockFile(names[0], key)
    if oerr != nil { t.Fatalf("open: %v", oerr) }
    defer r.Close()
    var got int
    for {
        var raw, rerr = r.next()
        if rerr != nil { t.Fatalf("next: %v", rerr) }
        if raw == nil { break }
        if len(raw) != len(blocks[got][0]) {
            t.Errorf("block %d is %d bytes, want %d", got, len(raw), len(blocks[got][0]))
        }
        if string(raw) != string(blocks[got][0]) {
            t.Errorf("block %d came back wrong — the obfuscation was not undone", got)
        }
        got++
    }
    if got != 3 { t.Errorf("read %d blocks, want 3", got) }
}

// A directory with no key file is an older node that wrote the blocks in the
// clear, which an all-zero key expresses.
func TestBlockReaderWithoutAKey(t *testing.T) {
    var dir = t.TempDir()
    if err := os.WriteFile(filepath.Join(dir, "blk00000.dat"), nil, 0600); err != nil {
        t.Fatalf("write: %v", err)
    }
    var key, err = xorKey(dir)
    if err != nil { t.Fatalf("key: %v", err) }
    for _, b := range key {
        if b != 0 { t.Fatalf("key = %x, want all zeros when xor.dat is absent", key) }
    }
}

// actbuild reads the block files, decides about each address once, and records
// the ones whose history is longer than the threshold.
func TestActbuildRecordsActiveAddresses(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000, blocks: chainFiles(t)}
    capture(t, func() { build(opt) })

    var oldMin = activeMin
    activeMin = 3
    defer func() { activeMin = oldMin }()
    var out = capture(t, func() { actbuild(opt) })
    if !strings.Contains(out, "Scanning") {
        t.Errorf("actbuild did not report its range: %q", out)
    }

    var active = activeAddresses(t)
    if _, ok := active[scriptAddress(payScript)]; ok {
        t.Errorf("the address with 2 transactions was recorded as active: %v", active)
    }
    // 6 touches: a coinbase in each of the four blocks, plus a spend and a
    // payment. One lookup both decides and counts, so this also pins that the
    // recorded figure is a real count and not the threshold.
    if n, ok := active[scriptAddress(otherScript)]; !ok || n != 6 {
        t.Errorf("active = %v; want %s with its 6 transactions", active, scriptAddress(otherScript))
    }
    // the cursor counts files, and the index's own is untouched
    if n, ok := addrindex.GetCursor(activeCursor); !ok || n != 1 {
        t.Errorf("actbuild cursor = %d, %v; want file 1", n, ok)
    }
    if h, _ := addrindex.Cursor(); h != 3 {
        t.Errorf("the index cursor moved to %d", h)
    }
}

// The in-memory set is the point: a script repeated across blocks and files is
// looked up once for the whole run.
func TestActbuildProcessesEachAddressOnce(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000, blocks: chainFiles(t)}
    capture(t, func() { build(opt) })
    var out = capture(t, func() { actbuild(opt) })
    // the fixture has two distinct scripts across four blocks in two files
    if !strings.Contains(out, "2 addresses looked up") {
        t.Errorf("want two lookups for two distinct scripts, got: %q", out)
    }
}

// A second run with no new files must do nothing, which is what the cursor buys.
func TestActbuildResumesFromItsCursor(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000, blocks: chainFiles(t)}
    capture(t, func() { build(opt) })
    capture(t, func() { actbuild(opt) })
    var again = capture(t, func() { actbuild(opt) })
    if !strings.Contains(again, "Every block file has been scanned") {
        t.Errorf("a second actbuild should be a no-op, got %q", again)
    }
}

// The set holds the whole 8-byte prefix, so two scripts are only ever confused
// if the index itself would confuse them.
func TestProcessedSet(t *testing.T) {
    var p = newProcessed(0)
    if !p.take(payScript) { t.Error("a script not seen before must be taken") }
    if p.take(payScript) { t.Error("the same script was taken twice") }
    if !p.take(otherScript) { t.Error("a different script must be taken") }
    if p.len() != 2 { t.Errorf("set holds %d, want 2", p.len()) }
    // the key is the index prefix, which is what makes the set agree with the
    // index about what counts as the same address
    p.flush()
    var want = binary.BigEndian.Uint64(addrindex.Prefix(payScript))
    if p.sorted[0] != want && p.sorted[1] != want {
        t.Error("the set is not keyed by the index prefix")
    }
}

// The merge is where a sorted set goes wrong, so it is driven hard: many keys,
// arriving out of order, across several flushes, with repeats throughout.
func TestProcessedSetMergesCorrectly(t *testing.T) {
    var old = bufferedAddrs
    bufferedAddrs = 16 // several flushes over a few hundred keys
    defer func() { bufferedAddrs = old }()

    var p = newProcessed(0)
    var want = map[uint64]bool{}
    var rnd uint64 = 12345
    for i := 0; i < 500; i++ {
        rnd = rnd*6364136223846793005 + 1442695040888963407
        var script = make([]byte, 8)
        binary.BigEndian.PutUint64(script, rnd%137) // a small range, so keys repeat
        var key = binary.BigEndian.Uint64(addrindex.Prefix(script))
        var fresh = p.take(script)
        if fresh == want[key] {
            t.Fatalf("take reported %v for a key already seen = %v", fresh, want[key])
        }
        want[key] = true
    }
    p.flush()
    if len(p.sorted) != len(want) {
        t.Errorf("set holds %d distinct keys, want %d", len(p.sorted), len(want))
    }
    for i := 1; i < len(p.sorted); i++ {
        if p.sorted[i-1] >= p.sorted[i] {
            t.Fatalf("the set is not sorted at %d: %d then %d", i, p.sorted[i-1], p.sorted[i])
        }
    }
    for k := range want {
        var i = sort.Search(len(p.sorted), func(i int) bool { return p.sorted[i] >= k })
        if i >= len(p.sorted) || p.sorted[i] != k {
            t.Fatalf("key %d was lost by a merge", k)
        }
    }
}

// Reserving room up front means the slice never reallocates, which is what
// keeps the peak at the size of the set rather than twice it.
func TestProcessedSetReserves(t *testing.T) {
    var p = newProcessed(1000)
    if cap(p.sorted) < 1000 {
        t.Errorf("reserved capacity %d, want at least 1000", cap(p.sorted))
    }
    var before = cap(p.sorted)
    var old = bufferedAddrs
    bufferedAddrs = 4
    defer func() { bufferedAddrs = old }()
    for i := 0; i < 100; i++ {
        var script = make([]byte, 8)
        binary.BigEndian.PutUint64(script, uint64(i))
        p.take(script)
    }
    p.flush()
    if cap(p.sorted) != before {
        t.Errorf("the slice reallocated (%d to %d) despite the reservation", before, cap(p.sorted))
    }
}

// Deciding and counting are one lookup, not two. Lookup walks the whole shard
// whichever limit it is given, so asking twice repeated the entire first call.
func TestClassifyMakesOneLookup(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000, blocks: chainFiles(t)}
    capture(t, func() { build(opt) })
    var oldMin = activeMin
    activeMin = 3
    defer func() { activeMin = oldMin }()
    capture(t, func() { actbuild(opt) })
    if n := activeAddresses(t)[scriptAddress(otherScript)]; n != 6 {
        t.Errorf("count = %d, want the real 6 — one lookup must both decide and count", n)
    }
}

// actbuild talks to no node at all. The addresses are encoded from the scripts
// locally, which was the last thing tying this pass to Core's RPC interface.
func TestActbuildMakesNoNodeRequests(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000, blocks: chainFiles(t)}
    capture(t, func() { build(opt) })
    var oldMin = activeMin
    activeMin = 1
    defer func() { activeMin = oldMin }()
    requests = 0
    capture(t, func() { actbuild(opt) })
    if requests != 0 {
        t.Errorf("actbuild made %d requests to the node; it should read only files", requests)
    }
    // and it still produced the addresses, encoded from the scripts themselves
    var active = activeAddresses(t)
    if len(active) != 2 {
        t.Errorf("recorded %d addresses, want both fixture scripts: %v", len(active), active)
    }
    if active[scriptAddress(payScript)] != 2 || active[scriptAddress(otherScript)] != 6 {
        t.Errorf("counts landed on the wrong addresses: %v", active)
    }
}

// actbuild has no source without it, so it must say so rather than scan nothing.
func TestActbuildNeedsABlocksDirectory(t *testing.T) {
    var _, err = blockFiles(t.TempDir())
    if err == nil {
        t.Error("an empty directory should not pass as a blocks directory")
    }
}

// The progress line carries a rate and, while there is chain left, an estimate.
func TestProgressReportsRateAndETA(t *testing.T) {
    var started = time.Now().Add(-10 * time.Second)
    // 100 blocks of 1000 done in 10s: 200 addr/sec, 900 blocks left at 10
    // blocks/sec is 90 seconds
    var got = progress(started, 1, 100, 1000, 2000)
    if !strings.Contains(got, "200 addr/sec") {
        t.Errorf("progress = %q, want a 200 addr/sec rate", got)
    }
    if !strings.Contains(got, "ETA") {
        t.Errorf("progress = %q, want an estimate while blocks remain", got)
    }
    // at the tip there is nothing left to estimate
    if got := progress(started, 1, 1000, 1000, 2000); strings.Contains(got, "ETA") {
        t.Errorf("progress = %q; there is no ETA once the scan is at the tip", got)
    }
}

func TestGroupAndRate(t *testing.T) {
    for _, c := range []struct {
        n    int64
        want string
    }{{0, "0"}, {7, "7"}, {999, "999"}, {1000, "1 000"}, {6177636, "6 177 636"}} {
        if got := group(c.n); got != c.want {
            t.Errorf("group(%d) = %q, want %q", c.n, got, c.want)
        }
    }
    if got := rate(3000, 2*time.Second); got != 1500 {
        t.Errorf("rate = %d, want 1500", got)
    }
    if got := rate(5, 0); got != 0 {
        t.Errorf("rate over no time = %d, want 0", got)
    }
}

// The flag overwrites the package var, so raising one without the other leaves
// the lower of the two in force — which is what happened when the var alone was
// raised to five million and -limit still defaulted to one.
func TestLookupLimitDefaultsAgree(t *testing.T) {
    var fs = flag.NewFlagSet("actbuild", flag.ContinueOnError)
    var opt = flags(fs)
    if err := fs.Parse(nil); err != nil { t.Fatalf("parse: %v", err) }
    if opt.limit != lookupLimit {
        t.Errorf("-limit defaults to %d but lookupLimit is %d; actbuild would use %d",
            opt.limit, lookupLimit, opt.limit)
    }
    if opt.limit != 5000000 {
        t.Errorf("-limit defaults to %d, want 5000000", opt.limit)
    }
}
