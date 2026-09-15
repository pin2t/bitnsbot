package main

import "encoding/binary"
import "fmt"
import "encoding/hex"
import "encoding/json"
import "io"
import "net/http"
import "net/http/httptest"
import "os"
import "path/filepath"
import "reflect"
import "strings"
import "sync/atomic"
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

// coinbaseSat is what every fixture block pays its miner, and opReturnScript is
// an output no address can be derived from — richbuild has to carry its coins in
// the balances it stores and leave them out of the rich table.
const coinbaseSat = 5000000000

var opReturnScript = mustHex("6a0b68656c6c6f20776f726c64")

// serialTx builds a non-segwit transaction with inputs inputs and one output
// per payment in pays. The input scripts are not written here: a block file is
// only ever read for its outputs.
//
// inputs is never 0 — a zero input count is the segwit marker, so even a
// coinbase carries one input, as it does on the real chain.
//
// version
//
// prevout hash + index
//
// empty scriptSig
//
// locktime
func serialTx(inputs int, pays []addrindex.Payment) []byte {
    var out []byte
    out = append(out, 1, 0, 0, 0)
    out = append(out, varint(inputs)...)
    for i := 0; i < inputs; i++ {
        out = append(out, make([]byte, 36)...)
        out = append(out, varint(0)...)
        out = append(out, 0xff, 0xff, 0xff, 0xff)
    }
    out = append(out, varint(len(pays))...)
    for _, p := range pays {
        var value = make([]byte, 8)
        binary.LittleEndian.PutUint64(value, uint64(p.Sat))
        out = append(out, value...)
        out = append(out, varint(len(p.Script))...)
        out = append(out, p.Script...)
    }
    out = append(out, 0, 0, 0, 0)
    return out
}

// blockTime is the fixture's clock: the real chain's genesis, then ten minutes a
// block. Every height has its own timestamp and a later block is always later,
// which is what ababuild ranks addresses by.
func blockTime(height int) int64 { return 1231006505 + int64(height)*600 }

// serialBlock is a block as Core's block files hold it. The header is otherwise
// zeroed, but it carries a real time at offset 68, since that is where a block
// file says when it happened.
//
// header
func serialBlock(height int, txs [][]byte) []byte {
    var out = make([]byte, 80)
    binary.LittleEndian.PutUint32(out[68:72], uint32(blockTime(height)))
    out = append(out, varint(len(txs))...)
    for _, t := range txs { out = append(out, t...) }
    return out
}

// chainBlocks is the fixture, indexed by height from genesis. Block 1 pays the
// address, block 2 spends from it and pays change back; 0 and 3 are unrelated.
// The amounts are the ones the fake node reports for the same transactions, so
// the balance richbuild sums out of the blocks and the history list resolves out
// of the node agree with each other: 20000 in, 20000 spent, 10000 back.
func chainBlocks() []addrindex.Block {
    var coinbase = addrindex.Tx{Outputs: []addrindex.Payment{{Script: otherScript, Sat: coinbaseSat}}}
    var block = func(height int, txs ...addrindex.Tx) addrindex.Block {
        return addrindex.Block{Hash: hashOfHeight(height), Time: blockTime(height), Txs: txs}
    }
    return []addrindex.Block{
        block(0, coinbase),
        block(1, coinbase, addrindex.Tx{
            Outputs: []addrindex.Payment{{Script: payScript, Sat: 20000}},
            Spent:   []addrindex.Payment{{Script: otherScript, Sat: coinbaseSat}}}),
        block(2, coinbase, addrindex.Tx{
            Outputs: []addrindex.Payment{{Script: otherScript, Sat: 10000}, {Script: payScript, Sat: 10000}, {Script: opReturnScript, Sat: 500}},
            Spent:   []addrindex.Payment{{Script: payScript, Sat: 20000}}}),
        block(3, coinbase),
    }
}

// serialChain is the same fixture as block files hold it, for actbuild.
func serialChain() [][]byte {
    var out [][]byte
    for h, b := range chainBlocks() {
        var txs [][]byte
        for _, tx := range b.Txs { txs = append(txs, serialTx(1, tx.Outputs)) }
        out = append(out, serialBlock(h, txs))
    }
    return out
}

// verboseBlock is what getblock at verbosity 3 reports for a fixture block,
// reduced to what the build reads: the time, and each transaction's outputs and
// inputs — a coinbase's input with no prevout, every other with one.
func verboseBlock(height int) map[string]interface{} {
    var b = chainBlocks()[height]
    var amount = func(p addrindex.Payment) map[string]interface{} {
        return map[string]interface{}{"value": float64(p.Sat) / 1e8, "scriptPubKey": map[string]string{"hex": hex.EncodeToString(p.Script)}}
    }
    var txs []interface{}
    for i, tx := range b.Txs {
        var vin, vout []interface{}
        if i == 0 { vin = append(vin, map[string]interface{}{"coinbase": "00"}) }
        for _, p := range tx.Spent { vin = append(vin, map[string]interface{}{"prevout": amount(p)}) }
        for _, p := range tx.Outputs { vout = append(vout, amount(p)) }
        txs = append(txs, map[string]interface{}{"vin": vin, "vout": vout})
    }
    return map[string]interface{}{"hash": b.Hash, "time": b.Time, "tx": txs}
}

// txidAt is the id the fake node reports for a given (height, position).
func txidAt(height uint32, index int) string {
    return strings.Repeat(string(rune('a'+height)), 63) + string(rune('0'+index))
}

// fakeChain is what the fake node calls itself. The fixture is a chain of its
// own, so it is not mainnet — which matters, because richbuild undoes three
// mainnet outputs that Core's UTXO set never held, and those heights are
// ordinary blocks here.
var fakeChain = "regtest"

// fakeCore is the node the tool talks to, over JSON-RPC alone: the builds read
// the chain through it and the lookups resolve transactions through it.
func fakeCore(t *testing.T, tip int) *httptest.Server {
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" {
            t.Errorf("unexpected request %s", r.URL.Path)
            w.WriteHeader(404)
            return
        }
        rpcReply(t, w, r, tip)
    }))
    t.Cleanup(srv.Close)
    return srv
}

// hashOfHeight and heightOfHash are the fixture's stand-in for real hashes: the
// height lives in the leading byte.
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
// a pass which should read only files touched the node not at all. The builds
// fetch blocks concurrently through it, so it is atomic.
var requests atomic.Int64

// Core answers a batch — an array of requests — with an array of results.
func rpcReply(t *testing.T, w http.ResponseWriter, r *http.Request, tip int) {
    requests.Add(1)
    var body, rerr = io.ReadAll(r.Body)
    if rerr != nil {
        t.Errorf("read rpc: %v", rerr)
        return
    }
    if len(body) > 0 && body[0] == '[' {
        var reqs []rpcCall
        if err := json.Unmarshal(body, &reqs); err != nil {
            t.Errorf("decode batch: %v", err)
            return
        }
        var out []map[string]interface{}
        for _, req := range reqs {
            out = append(out, map[string]interface{}{"id": batchID(req.ID), "result": answer(t, req, tip)})
        }
        json.NewEncoder(w).Encode(out)
        return
    }
    var req rpcCall
    if err := json.Unmarshal(body, &req); err != nil {
        t.Errorf("decode rpc: %v", err)
        return
    }
    json.NewEncoder(w).Encode(map[string]interface{}{"result": answer(t, req, tip)})
}

func answer(t *testing.T, req rpcCall, tip int) interface{} {
    var out interface{}
    var reply = func(v interface{}) { out = v }
    switch req.Method {
    case "getblockcount":
        reply(tip)
    case "getblockchaininfo":
        reply(map[string]interface{}{"blocks": tip, "chain": fakeChain})
    case "getchaintxstats":
        var txs int
        for _, b := range chainBlocks()[:tip+1] { txs += len(b.Txs) }
        reply(map[string]interface{}{"txcount": txs})
    case "validateaddress":
        reply(map[string]interface{}{"isvalid": true, "scriptPubKey": hex.EncodeToString(payScript)})
    case "getblockhash":
        reply(hashOfHeight(int(req.Params[0].(float64))))
    case "getblock":
        var height = uint32(heightOfHash(req.Params[0].(string)))
        if req.Params[1].(float64) == 3 {
            reply(verboseBlock(int(height)))
            break
        }
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

// The whole tool end to end: build the index from a fake node's blocks,
// then list an address out of it over the fake's JSON-RPC.
//
// the funding transaction, then the spend that paid change back
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
    if want := "21 oct 2021 21:00 bbbbbbbb..bbbbbbb1    20 000 sats"; lines[0] != want {
        t.Errorf("line 1 = %q, want %q", lines[0], want)
    }
    if want := "1 nov 2022 11:00  cccccccc..ccccccc1   -10 000 sats"; lines[1] != want {
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

// a spend is a negative line, so a column of them sums to the balance
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
    if got := amount(-10000); got != "-10 000 sats" {
        t.Errorf("amount = %q", got)
    }
}

// an empty history has no activity range to report
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
//
// Core preallocates, so the written records are followed by zeros — the
// reader has to stop there rather than read them as a record
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
    var blocks = serialChain()
    return writeBlockFiles(t, [][][]byte{
        {blocks[0], blocks[1]},
        {blocks[2], blocks[3]},
    })
}

// The reader has to undo Core's obfuscation, follow the magic-and-length
// framing, and stop at the padding rather than read it as a record.
func TestBlockReaderWalksAFile(t *testing.T) {
    var blocks = serialChain()
    var dir = writeBlockFiles(t, [][][]byte{{blocks[0], blocks[1], blocks[2]}})
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
        if len(raw) != len(blocks[got]) {
            t.Errorf("block %d is %d bytes, want %d", got, len(raw), len(blocks[got]))
        }
        if string(raw) != string(blocks[got]) {
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

// Real blocks from a Bitcoin Core v31.1.0 regtest node, as its REST interface
// served them serialized (/rest/block/<hash>.bin), with the per-transaction
// output scripts Core itself reported via getblock verbosity 3 for the same
// blocks. block102 has one coinbase and one spend; block103 has a coinbase and
// two further spends chained off it. Parsing has to agree with the node exactly,
// since a mismatch means silently missing or fabricating address history.
func TestParseBlock(t *testing.T) {
    for _, c := range []struct {
        name  string
        block string
        want  [][]string
    }{
        {"block102", "000000202b687778eddbeeb89f1551e63aa7505b461f396f09e3b92f23ba83aa221c243a416b1476753995b3ce8b34a9de6729c37f50b1c4576e53e1fb9eb7f5c25bceb6e7205e6affff7f200000000002020000000001010000000000000000000000000000000000000000000000000000000000000000ffffffff03016600feffffff0204fd052a01000000160014d2b2f31918bdd57ca878317b5c639ec0a739e2690000000000000000266a24aa21a9ed27790064491a6e6e5ebc94766e0e173b5c62be6ab9c3cd133c32b2c7284f4137012000000000000000000000000000000000000000000000000000000000000000006500000002000000000101c08f084fd33d0a5207255c89e0340417dc4fe0cb8acbf26b345a202667abdc630000000000fdffffff027c15152101000000160014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec80d1f00800000000160014a6d49256e4f923822286832a77d7e2a909a4c7620247304402204a75bca110a1800c02566bf94f2ef7705902ea1df66280361d9a61543e4a2ec1022007351332e5630462fe83972dafb74a4614a43bc613834a707daed17221ff6612012102cccf1e47f3a6326ed4101c7185542a2844fd25076390555dfc04705ecf48468f65000000",
            [][]string{{"0014d2b2f31918bdd57ca878317b5c639ec0a739e269", "6a24aa21a9ed27790064491a6e6e5ebc94766e0e173b5c62be6ab9c3cd133c32b2c7284f4137"}, {"0014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec", "0014a6d49256e4f923822286832a77d7e2a909a4c762"}}},
        {"block103", "000000209adb0270161c90be20851202870014d8d27d98e1832ed960dea0392a1ae7292d13e5e72d08a2dbd9e678ece47c21644f037796143766b3161c15b3ee79ae9aca51215e6affff7f200000000003020000000001010000000000000000000000000000000000000000000000000000000000000000ffffffff03016700feffffff020808062a01000000160014d2b2f31918bdd57ca878317b5c639ec0a739e2690000000000000000266a24aa21a9ed00b1a6de804a7485ba221e46946f767afacc26540da46079027797aad8f31c8f012000000000000000000000000000000000000000000000000000000000000000006600000002000000000101e538de5326756d73685cfac914d6675b149ede18b5f4dfb06f37f90773cde8f40000000000fdffffff0240787d010000000016001467efcb63b3a1135979ecc3a00956ef07dea371cc3892971f01000000160014168a5d01b286b82426aa34edd402322c1d7b3cd602473044022015a670c5430ba3b2c770f0471bd6d3d30185d65fe32bfa239f895aff3f0882f20220354a61aeeadb75f694269647ca75efb63b8826ad3e50e831b90bd4e25ccdec72012103c0c512d5d970876c6ec681c298930265d899985cef0b58f230ef0d74acbfd3d26600000002000000000101e538de5326756d73685cfac914d6675b149ede18b5f4dfb06f37f90773cde8f40100000000fdffffff0280f0fa0200000000160014705e4a46bed92fff46ad9a1eab968a637e8055b8fcd5f505000000001600142cdf30a44b7eb15da3346626e3ae5c9c3aa09607024730440220723dd8e9aec6f5a2f8acc7476a1864de5b99b86a27a2f245c26368538e1798630220271c7b873e5c56ada876832b620c36186629d53f67312ca4e41e0eac4deb89c501210360a37afc68b928733d6891eb33f3865799b7cd2699583175e6d9fc9c3e9f670f66000000",
            [][]string{{"0014d2b2f31918bdd57ca878317b5c639ec0a739e269", "6a24aa21a9ed00b1a6de804a7485ba221e46946f767afacc26540da46079027797aad8f31c8f"}, {"001467efcb63b3a1135979ecc3a00956ef07dea371cc", "0014168a5d01b286b82426aa34edd402322c1d7b3cd6"}, {"0014705e4a46bed92fff46ad9a1eab968a637e8055b8", "00142cdf30a44b7eb15da3346626e3ae5c9c3aa09607"}}},
    } {
        var outputs, ok = parseBlockOutputs(mustHex(c.block))
        if !ok { t.Fatalf("%s: parseBlockOutputs failed", c.name) }
        if got := hexLists(outputs); !reflect.DeepEqual(got, c.want) {
            t.Errorf("%s: outputs = %v, want %v", c.name, got, c.want)
        }
    }
}

func hexLists(scripts [][]addrindex.Payment) [][]string {
    var out = make([][]string, len(scripts))
    for i, list := range scripts {
        out[i] = make([]string, len(list))
        for j, o := range list {
            out[i][j] = hex.EncodeToString(o.Script)
        }
    }
    return out
}

// The amounts too, over two more regtest blocks as REST served them
// (testdata/block<h>.hex), against what getblock verbosity 3 reported for the
// same blocks: genesis, whose coinbase pays a bare public key, and block 113,
// whose second transaction spends P2PKH, P2SH, P2WPKH and taproot outputs at
// once and pays an OP_RETURN. The addrindex package holds its RPC decoding to
// these same outputs.
func TestParseBlockReadsAmounts(t *testing.T) {
    for height, want := range map[int][][]addrindex.Payment{
        0: {{{Script: mustHex("4104678afdb0fe5548271967f1a67130b7105cd6a828e03909a67962e0ea1f61deb649f6bc3f4cef38c4f35504e51ec112de5c384df7ba0b8d578a4c702b6bf11d5fac"), Sat: 5000000000}}},
        113: {{
            {Script: mustHex("a914009a5f4eca9506b9fe82ae824a2d2187d9fc96a187"), Sat: 5000009300},
            {Script: mustHex("6a24aa21a9ed1e1342cf2e2a885b117c9ec3bde85adb93c7a1276553ab5fb2bd59ee52880b3e"), Sat: 0},
        }, {
            {Script: mustHex("76a914203b872d7db07e5dd6eac63603bad4432c57907688ac"), Sat: 50000000},
            {Script: mustHex("51200738980554d28706a692599b8a6acb74a98bbb71c497dd0c0836194a82d923e0"), Sat: 1314990700},
            {Script: mustHex("6a03626974"), Sat: 0},
        }},
    } {
        var raw, err = os.ReadFile(filepath.Join("testdata", fmt.Sprintf("block%d.hex", height)))
        if err != nil { t.Fatalf("fixture: %v", err) }
        var outputs, ok = parseBlockOutputs(mustHex(string(raw)))
        if !ok { t.Fatalf("block %d: parseBlockOutputs failed", height) }
        if !reflect.DeepEqual(outputs, want) {
            t.Errorf("block %d: outputs = %v, want %v", height, outputs, want)
        }
    }
}

// actbuild reads the block files, counts the transactions paying to each
// address, and records the ones past the threshold.
//
// a threshold of 3: the fixture pays otherScript in four transactions (a
// coinbase in each block plus one more) and payScript in two
//
// five: a coinbase paying otherScript in each of the four blocks, plus
// block 2's transaction paying it as well
func TestActbuildCountsFromBlocks(t *testing.T) {
    openIndex(t)
    var opt = &options{limit: 1000, blocks: chainFiles(t), active: 3}
    var oldMin = activeMin
    activeMin = 3
    defer func() { activeMin = oldMin }()
    var out = capture(t, func() { actbuild(opt) })
    if !strings.Contains(out, "Scanning") {
        t.Errorf("actbuild did not report what it was doing: %q", out)
    }
    var active = activeAddresses(t)
    if _, ok := active[addrindex.Address(payScript)]; ok {
        t.Errorf("the address in 2 transactions was recorded as active: %v", active)
    }
    if n, ok := active[addrindex.Address(otherScript)]; !ok || n != 5 {
        t.Errorf("active = %v; want %s in 5 transactions", active, addrindex.Address(otherScript))
    }
}

// actbuild reads the files and nothing else — no index lookups, no node.
func TestActbuildMakesNoNodeRequests(t *testing.T) {
    openIndex(t)
    var opt = &options{limit: 1000, blocks: chainFiles(t), active: 1}
    var oldMin = activeMin
    activeMin = 1
    defer func() { activeMin = oldMin }()
    requests.Store(0)
    capture(t, func() { actbuild(opt) })
    if n := requests.Load(); n != 0 {
        t.Errorf("actbuild made %d requests to the node; it should read only files", n)
    }
    if len(activeAddresses(t)) != 2 {
        t.Errorf("recorded %v, want both fixture addresses", activeAddresses(t))
    }
}

// It does not need the index at all, so an empty one must not stop it.
func TestActbuildNeedsNoIndex(t *testing.T) {
    openIndex(t)
    var opt = &options{limit: 1000, blocks: chainFiles(t), active: 1}
    var oldMin = activeMin
    activeMin = 1
    defer func() { activeMin = oldMin }()
    capture(t, func() { actbuild(opt) })
    if len(activeAddresses(t)) == 0 {
        t.Error("actbuild produced nothing against an unbuilt index; it should not need one")
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
//
// 100 blocks of 1000 done in 10s: 200 addr/sec, 900 blocks left at 10
// blocks/sec is 90 seconds
//
// at the tip there is nothing left to estimate
func TestProgressReportsRateAndETA(t *testing.T) {
    var started = time.Now().Add(-10 * time.Second)
    var got = progress(started, 1, 100, 1000, 2000)
    if !strings.Contains(got, "200 addr/sec") {
        t.Errorf("progress = %q, want a 200 addr/sec rate", got)
    }
    if !strings.Contains(got, "ETA") {
        t.Errorf("progress = %q, want an estimate while blocks remain", got)
    }
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
