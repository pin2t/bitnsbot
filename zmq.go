package main

import "context"
import "encoding/binary"
import "encoding/hex"
import "fmt"
import "strings"
import "sync"

import "github.com/go-zeromq/zmq4"
import "bitnsbot/logging"

// Bitcoin Core has no server-side address filter — nothing like btcd's
// loadtxfilter — so ZMQ delivers *every* mempool transaction and the matching
// happens here. That is why the raw transaction is parsed locally instead of
// being handed straight to decoderawtransaction: on mainnet this path runs
// several times a second, and an RPC round trip per transaction (let alone the
// prevout fetches) would be wasteful when almost none of them are interesting.
//
// Matching never needs to decode an address: watched addresses are turned into
// their scriptPubKey once, by validateaddress, and outputs are compared as raw
// script bytes. Inputs are matched by outpoint, exactly as btcd's filter did
// internally — see watched.go for how that set is seeded and kept up to date.

// outpoint identifies one spendable output.
type outpoint struct {
    txid string
    vout uint32
}

var watchedMu sync.Mutex
var watchedScripts = make(map[string]string)   // scriptPubKey hex → address
var watchedOutpoints = make(map[outpoint]string) // outpoint → the address that owns it

func watchScript(script, address string) {
    if script == "" { return }
    watchedMu.Lock()
    watchedScripts[script] = address
    watchedMu.Unlock()
}

func watchOutpoint(op outpoint, address string) {
    watchedMu.Lock()
    watchedOutpoints[op] = address
    watchedMu.Unlock()
}

func unwatchScripts(address string) {
    watchedMu.Lock()
    for script, addr := range watchedScripts {
        if addr == address { delete(watchedScripts, script) }
    }
    for op, addr := range watchedOutpoints {
        if addr == address { delete(watchedOutpoints, op) }
    }
    watchedMu.Unlock()
}

func resetWatched() {
    watchedMu.Lock()
    watchedScripts = make(map[string]string)
    watchedOutpoints = make(map[outpoint]string)
    watchedMu.Unlock()
}

func anyWatched() bool {
    watchedMu.Lock()
    defer watchedMu.Unlock()
    return len(watchedScripts) > 0 || len(watchedOutpoints) > 0
}

// matches reports whether a parsed transaction pays to or spends from any
// watched address. It is a pure predicate: the outpoints a transaction *creates*
// are recorded later by recordOutpoints, on the RPC path, because that is where
// the txid needed to key them is known.
func matches(tx *parsedTx) bool {
    watchedMu.Lock()
    defer watchedMu.Unlock()
    for _, in := range tx.inputs {
        if _, ok := watchedOutpoints[in]; ok { return true }
    }
    for _, script := range tx.outputScripts {
        if _, ok := watchedScripts[script]; ok { return true }
    }
    return false
}

// recordOutpoints remembers every output of a decoded transaction that pays a
// watched address, so that spending it later is recognised — the same
// bookkeeping btcd did inside its own filter, which the bot now has to do for
// itself. Keyed by scriptPubKey so it never depends on address formatting.
func recordOutpoints(txid string, vouts []coreVout) {
    watchedMu.Lock()
    defer watchedMu.Unlock()
    for _, v := range vouts {
        if addr, ok := watchedScripts[v.ScriptPubKey.Hex]; ok {
            watchedOutpoints[outpoint{txid, v.N}] = addr
        }
    }
}

// parsedTx is the little that matching needs from a serialized transaction: the
// outpoints it spends and the scripts it pays.
type parsedTx struct {
    inputs        []outpoint
    outputScripts []string
}

// parseTx walks a serialized transaction far enough to read its inputs and
// outputs, skipping the witness and locktime it has no use for. It deliberately
// does not compute the txid (that needs a double SHA-256 over the non-witness
// serialization); the txid is filled in by the caller from the ZMQ topic or the
// RPC path, and is only used to key the outpoints this transaction creates.
func parseTx(raw []byte) (*parsedTx, bool) {
    var r = &reader{buf: raw}
    r.skip(4) // version
    var count, ok = r.varInt()
    if !ok { return nil, false }
    if count == 0 { // segwit marker: the real input count follows the flag byte
        r.skip(1)
        count, ok = r.varInt()
        if !ok { return nil, false }
    }
    var tx = &parsedTx{}
    for i := uint64(0); i < count; i++ {
        var hash, hashOK = r.bytes(32)
        var index, indexOK = r.uint32()
        if !hashOK || !indexOK { return nil, false }
        var scriptLen, lenOK = r.varInt()
        if !lenOK { return nil, false }
        r.skip(int(scriptLen))
        r.skip(4) // sequence
        tx.inputs = append(tx.inputs, outpoint{reverseHex(hash), index})
    }
    var outCount, outOK = r.varInt()
    if !outOK { return nil, false }
    for i := uint64(0); i < outCount; i++ {
        r.skip(8) // value
        var scriptLen, lenOK = r.varInt()
        if !lenOK { return nil, false }
        var script, scriptOK = r.bytes(int(scriptLen))
        if !scriptOK { return nil, false }
        tx.outputScripts = append(tx.outputScripts, hex.EncodeToString(script))
    }
    if r.bad { return nil, false }
    return tx, true
}

// reverseHex renders a 32-byte hash the way Bitcoin displays it: serialized
// little-endian on the wire, printed big-endian.
func reverseHex(b []byte) string {
    var flipped = make([]byte, len(b))
    for i := range b { flipped[i] = b[len(b)-1-i] }
    return hex.EncodeToString(flipped)
}

type reader struct {
    buf []byte
    pos int
    bad bool
}

func (r *reader) skip(n int) {
    if r.pos+n > len(r.buf) { r.bad = true; return }
    r.pos += n
}

func (r *reader) bytes(n int) ([]byte, bool) {
    if n < 0 || r.pos+n > len(r.buf) { r.bad = true; return nil, false }
    var out = r.buf[r.pos : r.pos+n]
    r.pos += n
    return out, true
}

func (r *reader) uint32() (uint32, bool) {
    var b, ok = r.bytes(4)
    if !ok { return 0, false }
    return binary.LittleEndian.Uint32(b), true
}

// varInt reads Bitcoin's variable-length integer. Unlike the advertise tool —
// which only ever writes values below 0xfd — all four forms are reachable here,
// since transaction input/output counts and script lengths are attacker-chosen.
func (r *reader) varInt() (uint64, bool) {
    var first, ok = r.bytes(1)
    if !ok { return 0, false }
    switch first[0] {
    case 0xfd:
        var b, ok = r.bytes(2)
        if !ok { return 0, false }
        return uint64(binary.LittleEndian.Uint16(b)), true
    case 0xfe:
        var b, ok = r.bytes(4)
        if !ok { return 0, false }
        return uint64(binary.LittleEndian.Uint32(b)), true
    case 0xff:
        var b, ok = r.bytes(8)
        if !ok { return 0, false }
        return binary.LittleEndian.Uint64(b), true
    default:
        return uint64(first[0]), true
    }
}

// startZMQ subscribes to Core's block and mempool notifications. Losing messages
// while the bot is down is accepted: the block cache backfills on startup and
// pending confirmation watches were never persisted anyway.
func startZMQ(ctx context.Context, endpoints []string, b *bot) error {
    if len(endpoints) == 0 { return fmt.Errorf("no ZMQ endpoints configured") }
    var sub = zmq4.NewSub(ctx)
    // Core publishes each topic on whatever address its own -zmqpub* option
    // names, so the topics we want may live on one port or on several. A SUB
    // socket can dial every publisher and receive from all of them, and
    // subscribing to a topic an endpoint never publishes simply yields nothing.
    for _, endpoint := range endpoints {
        if err := sub.Dial(endpoint); err != nil { return fmt.Errorf("dial %s: %w", endpoint, err) }
    }
    for _, topic := range []string{"hashblock", "rawtx"} {
        if err := sub.SetOption(zmq4.OptionSubscribe, topic); err != nil { return err }
    }
    go func() {
        defer sub.Close()
        for {
            var msg, err = sub.Recv()
            if err != nil {
                if ctx.Err() != nil { return }
                logging.Warn("zmq: %v", err)
                continue
            }
            if len(msg.Frames) < 2 { continue }
            switch string(msg.Frames[0]) {
            case "hashblock":
                var hash = hex.EncodeToString(msg.Frames[1])
                go processBlock(hash)
                go processConfirms(b, hash)
                go onNewBlock()
            case "rawtx":
                if !anyWatched() { continue }
                var tx, ok = parseTx(msg.Frames[1])
                if !ok {
                    logging.Warn("zmq: could not parse a %d-byte transaction", len(msg.Frames[1]))
                    return
                }
                if !matches(tx) { continue }
                go broadcast(hex.EncodeToString(msg.Frames[1]))
            }
        }
    }()
    logging.Status("zmq: subscribed to Bitcoin Core notifications at %s", strings.Join(endpoints, ", "))
    return nil
}