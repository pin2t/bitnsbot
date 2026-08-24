package main

import "encoding/binary"
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

func rpcReply(t *testing.T, w http.ResponseWriter, r *http.Request) {
    var req struct {
        Method string        `json:"method"`
        Params []interface{} `json:"params"`
    }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil { t.Fatalf("decode rpc: %v", err) }
    var reply = func(v interface{}) { json.NewEncoder(w).Encode(map[string]interface{}{"result": v}) }
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
    var db, err = bbolt.Open(filepath.Join(t.TempDir(), "ai.db"), 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    t.Cleanup(func() { db.Close() })
    if err := addrindex.Init(db); err != nil { t.Fatalf("init: %v", err) }
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
