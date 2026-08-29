package main

import "encoding/binary"
import "flag"
import "encoding/hex"
import "encoding/json"
import "io"
import "net/http"
import "net/http/httptest"
import "os"
import "path/filepath"
import "strconv"
import "strings"
import "testing"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"

// The script the fixture's address is paid to. Its bytes are all that matter —
// the index is keyed by scriptPubKey, and no address format is ever decoded.
var payScript = mustHex("76a914000102030405060708090a0b0c0d0e0f1011121314ff88ac")
var otherScript = mustHex("76a914aabbccddeeff00112233445566778899aabbccdd88ac")

const address = "37QAiiRLSHEsMPu3SXT9AKWDoZsZxtfuRP"
const otherAddress = "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"

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

// batched counts the batch requests the fake has served, so a test can assert
// that a group of scripts cost one round trip rather than one each.
var batched int

func rpcReply(t *testing.T, w http.ResponseWriter, r *http.Request) {
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
        batched++
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
    case "decodescript":
        // the fixture's two scripts map to two addresses; anything else is
        // nonstandard and has none
        switch req.Params[0].(string) {
        case hex.EncodeToString(payScript):
            reply(map[string]interface{}{"address": address})
        case hex.EncodeToString(otherScript):
            reply(map[string]interface{}{"address": otherAddress})
        default:
            reply(map[string]interface{}{})
        }
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

// actbuild walks the chain, decides about each address once, and records the
// ones whose history is longer than the threshold. The fixture's payScript has
// two transactions and otherScript four, so a threshold of three separates them.
func TestActbuildRecordsActiveAddresses(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000}
    capture(t, func() { build(opt) })

    var oldMin = activeMin
    activeMin = 3
    defer func() { activeMin = oldMin }()
    var out = capture(t, func() { actbuild(opt) })
    if !strings.Contains(out, "Scanning blocks 0..3") {
        t.Errorf("actbuild did not report its range: %q", out)
    }

    var active = activeAddresses(t)
    if _, ok := active[address]; ok {
        t.Errorf("the address with 2 transactions was recorded as active: %v", active)
    }
    // 6 touches: a coinbase in each of the four blocks, plus a spend and a
    // payment. The deciding lookup stops at the threshold, so this also pins
    // that the recorded count is a real count and not that cap.
    if n, ok := active[otherAddress]; !ok || n != 6 {
        t.Errorf("active = %v; want %s with its 6 transactions", active, otherAddress)
    }
    if h, ok := addrindex.GetCursor(activeCursor); !ok || h != 3 {
        t.Errorf("actbuild cursor = %d, %v; want 3", h, ok)
    }
    // the index's own cursor is untouched — they sit in one bucket under
    // different names
    if h, _ := addrindex.Cursor(); h != 3 {
        t.Errorf("the index cursor moved to %d", h)
    }
}

// The processed set is the point of the exercise: an address decided about in an
// earlier chunk must not be looked up again in a later one.
func TestActbuildProcessesEachAddressOnce(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000}
    capture(t, func() { build(opt) })
    var oldChunk = actChunk
    actChunk = 1 // one block per chunk, so the repeats land in later chunks
    defer func() { actChunk = oldChunk }()
    capture(t, func() { actbuild(opt) })

    // both scripts appear in several blocks, but each is recorded once
    for _, script := range [][]byte{payScript, otherScript} {
        var shard, rem = shardKey(script)
        var count int
        db.View(func(tx *bbolt.Tx) error {
            var v = tx.Bucket(processedBucket).Get(shard)
            for i := 0; i+remainderLen <= len(v); i += remainderLen {
                if string(v[i:i+remainderLen]) == string(rem) { count++ }
            }
            return nil
        })
        if count != 1 {
            t.Errorf("script %x recorded %d times in the processed set, want 1", script[:8], count)
        }
    }
}

// A second run with nothing new must do nothing, which is what the cursor buys.
func TestActbuildResumesFromItsCursor(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000}
    capture(t, func() { build(opt) })
    capture(t, func() { actbuild(opt) })
    var again = capture(t, func() { actbuild(opt) })
    if !strings.Contains(again, "already up to the tip") {
        t.Errorf("a second actbuild should be a no-op, got %q", again)
    }
}

// The sharded set is the whole storage design, so its parts are pinned
// directly: 2 bytes of shard and 6 of remainder, the same split the index uses,
// packed into a run that is kept sorted so membership can binary-search it.
func TestProcessedShardLayout(t *testing.T) {
    var shard, rem = shardKey(payScript)
    if len(shard) != 2 || len(rem) != 6 {
        t.Fatalf("shard %d bytes, remainder %d; want 2 and 6", len(shard), len(rem))
    }
    var prefix = addrindex.Prefix(payScript)
    if string(shard) != string(prefix[:2]) || string(rem) != string(prefix[2:8]) {
        t.Error("the split must be the first 2 and last 6 bytes of the index prefix")
    }
}

// run builds a sorted shard value out of whole remainders.
func run(remainders ...[]byte) []byte {
    var out []byte
    for _, r := range remainders { out = append(out, r...) }
    return out
}

func rem6(b byte) []byte { return []byte{b, 0, 0, 0, 0, 0} }

// run2 is run under another name, for tests that shadow run with a local.
func run2(remainders ...[]byte) []byte { return run(remainders...) }

// Membership is a binary search, so it must find an entry wherever it sits in
// the run and never claim one that is not there.
func TestSeenBinarySearch(t *testing.T) {
    var shard = run(rem6(1), rem6(3), rem6(5), rem6(7), rem6(9))
    for _, b := range []byte{1, 3, 5, 7, 9} {
        if !seen(shard, rem6(b)) {
            t.Errorf("%d is in the run but was not found", b)
        }
    }
    for _, b := range []byte{0, 2, 4, 6, 8, 10} {
        if seen(shard, rem6(b)) {
            t.Errorf("%d is not in the run but was found", b)
        }
    }
    if seen(nil, rem6(1)) {
        t.Error("an empty shard cannot contain anything")
    }
    // a remainder must not match off an entry boundary, which a byte search would
    if seen(run(rem6(0))[1:], rem6(0)) {
        t.Error("a remainder was matched off the entry boundary")
    }
}

// insertOne puts a remainder in its place rather than at the end, so the run
// stays sorted and stays searchable.
func TestInsertOneKeepsTheRunSorted(t *testing.T) {
    var run = run(rem6(2), rem6(6))
    // one before, one between, one after — each lands where the order requires
    for _, b := range []byte{8, 1, 4} {
        run = insertOne(run, rem6(b))
    }
    var want = run2(rem6(1), rem6(2), rem6(4), rem6(6), rem6(8))
    if string(run) != string(want) {
        t.Fatalf("run = %x, want %x", run, want)
    }
    for _, b := range []byte{1, 2, 4, 6, 8} {
        if !seen(run, rem6(b)) {
            t.Errorf("%d was lost by the insert", b)
        }
    }
}

// Re-inserting something already there must not double it, or the set would
// grow without bound on a re-scan.
func TestInsertOneDoesNotDuplicate(t *testing.T) {
    var r = run(rem6(2), rem6(4))
    r = insertOne(r, rem6(4))
    r = insertOne(r, rem6(2))
    if got := len(r) / remainderLen; got != 2 {
        t.Errorf("run holds %d entries, want 2", got)
    }
    // into an empty run
    if got := insertOne(nil, rem6(5)); string(got) != string(rem6(5)) {
        t.Errorf("into an empty run: %x", got)
    }
}

// merge combines two sorted runs, which is how a chunk's additions reach the
// stored shard.
func TestMergeSortedRuns(t *testing.T) {
    var stored = run(rem6(2), rem6(5), rem6(9))
    var batch = run(rem6(1), rem6(5), rem6(7))
    var got = merge(stored, batch)
    var want = run2(rem6(1), rem6(2), rem6(5), rem6(7), rem6(9))
    if string(got) != string(want) {
        t.Fatalf("merge = %x, want %x — 5 is in both and must be kept once", got, want)
    }
    // either side empty
    if string(merge(nil, batch)) != string(batch) {
        t.Error("merging into an empty run lost the batch")
    }
    if string(merge(stored, nil)) != string(stored) {
        t.Error("merging an empty batch changed the run")
    }
    // one side entirely past the other, so the tail copy finishes it
    if got := merge(run(rem6(1)), run(rem6(8), rem6(9))); string(got) != string(run2(rem6(1), rem6(8), rem6(9))) {
        t.Errorf("appending past the end: %x", got)
    }
}

// Deciding and counting are one lookup, not two. Lookup walks the whole shard
// whichever limit it is given, so asking twice repeated the entire first call.
func TestClassifyMakesOneLookup(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000}
    capture(t, func() { build(opt) })
    var oldMin = activeMin
    activeMin = 3
    defer func() { activeMin = oldMin }()
    capture(t, func() { actbuild(opt) })
    // the count recorded is the real one, which a deciding-only lookup capped at
    // the threshold could not produce
    if n := activeAddresses(t)[otherAddress]; n != 6 {
        t.Errorf("count = %d, want the real 6 — one lookup must both decide and count", n)
    }
}

// The scripts that qualify are resolved in one batched call rather than a round
// trip each.
func TestDecodeScriptIsBatched(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000}
    capture(t, func() { build(opt) })
    var oldMin, oldBatch = activeMin, decodeBatch
    // a threshold low enough that both fixture scripts qualify, so there is
    // something to batch
    activeMin, decodeBatch = 1, 1000
    defer func() { activeMin, decodeBatch = oldMin, oldBatch }()
    batched = 0
    capture(t, func() { actbuild(opt) })
    if batched == 0 {
        t.Fatal("decodescript was never sent as a batch")
    }
    var active = activeAddresses(t)
    if len(active) != 2 {
        t.Errorf("resolved %d addresses, want both fixture scripts: %v", len(active), active)
    }
    // and the counts must still line up with the right addresses, which is what
    // matching a batch reply by id is for
    if active[address] != 2 || active[otherAddress] != 6 {
        t.Errorf("counts landed on the wrong addresses: %v", active)
    }
}

// A batch bigger than decodeBatch is split, and every script still comes back
// against its own count.
func TestDecodeScriptSplitsLargeBatches(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000}
    capture(t, func() { build(opt) })
    var oldMin, oldBatch = activeMin, decodeBatch
    activeMin, decodeBatch = 1, 1 // one script per call, so two calls
    defer func() { activeMin, decodeBatch = oldMin, oldBatch }()
    batched = 0
    capture(t, func() { actbuild(opt) })
    if batched < 2 {
        t.Errorf("sent %d batches for two scripts at decodeBatch 1, want at least 2", batched)
    }
    var active = activeAddresses(t)
    if active[address] != 2 || active[otherAddress] != 6 {
        t.Errorf("splitting the batch lost the pairing: %v", active)
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
