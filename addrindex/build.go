package addrindex

import "context"
import "encoding/binary"
import "time"

import "bitnsbot/logging"

// Block is the raw material Blockchain hands over for one height: the serialized
// block (as Core's REST /rest/block/<hash>.bin returns it) and the serialized
// spent-outputs data (/rest/spenttxouts/<hash>.bin) — the prevout of every real
// input in the block, aligned to the block's own transaction order. Together
// they carry everything a touch needs (which scripts each transaction pays, and
// which scripts it spends from) without decoding a single txid: build() never
// computes one, because a touch is keyed by (height, tx index in block), not by
// txid — see addrindex.go.
type Block struct {
    Hash  string
    Raw   []byte
    Spent []byte
}

// Blockchain supplies chain data to the backfill; the caller (package main) owns the
// HTTP/REST specifics, mirroring how the miners package takes its chain data
// through a Blockchain interface because it can't reach btcd/Core directly either.
type Blockchain interface {
    Tip(ctx context.Context) (int, error)
    BlockAt(ctx context.Context, height int) (Block, error)
}

// chunkSize bounds how many blocks are merged into the index per bbolt
// transaction, the same reasoning as the miners collector: catching up hundreds
// of thousands of blocks must not build one giant transaction, and a crash
// mid-catch-up should resume from the last flushed chunk, not the beginning. A
// package var so tests shrink it.
var chunkSize int = 1000

// backfillInterval is the pause between catch-up passes once the index is at the
// tip, so new blocks are picked up without a dedicated subscription.
var backfillInterval = 2 * time.Minute

// StartBackfill walks the chain from the index's cursor to the tip, in chunks,
// and keeps polling for new blocks afterward. It is meant to run for as long as
// the bot does; building genesis-to-tip on a fresh index is a multi-hour, one-
// time cost paid the same way the miners collector pays its own catch-up.
func StartBackfill(src Blockchain) {
    go func() {
        for {
            if err := Build(src); err != nil {
                logging.Warn("addrindex: %v", err)
            }
            time.Sleep(backfillInterval)
        }
    }()
}

// Build walks the chain from the index's cursor to the tip once and returns.
// StartBackfill is this on a loop; tools/addrindex calls it directly, so a
// command-line build and the bot's own backfill advance the same cursor through
// the same chunking.
func Build(src Blockchain) error {
    var ctx, cancel = context.WithTimeout(context.Background(), 6*time.Hour)
    defer cancel()
    var tip, err = src.Tip(ctx)
    if err != nil { return err }
    var height, ok = Cursor()
    var from int
    if ok { from = height + 1 }
    for from <= tip {
        var to = from + chunkSize - 1
        if to > tip { to = tip }
        var touches = map[string][]Touch{}
        for h := from; h <= to; h++ {
            var blk, berr = src.BlockAt(ctx, h)
            if berr != nil { return berr }
            indexBlock(touches, uint32(h), blk)
        }
        if err := merge(touches, to); err != nil { return err }
        logging.Info("addrindex: built blocks %d..%d (tip %d)", from, to, tip)
        from = to + 1
    }
    return nil
}

// Payment is a scriptPubKey and the satoshi paid to it — one output of a
// transaction, or one prevout an input spends. The index needs only the script;
// a balance needs the amount too, so the parsers below read both and each caller
// takes what it uses.
type Payment struct {
    Script []byte
    Sat    int64
}

// indexBlock extracts every touch in one block into the running chunk map: every
// output's script is a funding touch, every spent input's prevout script (read
// positionally from Spent, aligned to the block's transaction order) is a
// spending touch. Both are gathered per transaction and deduplicated there, so a
// transaction touching the same address more than once — two outputs to one
// address, or an address appearing in both an input and an output of the same
// transaction — is recorded once. parseBlockOutputs and parseSpentOutputs are
// guaranteed to return one slice per transaction in the same order, so index i
// in each always describes the same transaction.
func indexBlock(touches map[string][]Touch, height uint32, blk Block) {
    var outputs, ok1 = parseBlockOutputs(blk.Raw)
    var spent, ok2 = parseSpentOutputs(blk.Spent)
    if !ok1 || !ok2 || len(outputs) != len(spent) {
        logging.Warn("addrindex: could not parse block %s at height %d", blk.Hash, height)
        return
    }
    indexBlockFromParsed(touches, height, outputs, spent)
}

// indexBlockFromParsed is indexBlock's dedup logic, split out so it can be
// tested directly against constructed script lists without needing a real
// serialized block for every case.
func indexBlockFromParsed(touches map[string][]Touch, height uint32, outputs, spent [][]Payment) {
    for txIndex := range outputs {
        var seen = map[string]bool{}
        for _, o := range outputs[txIndex] {
            if p := string(Prefix(o.Script)); len(o.Script) > 0 && !seen[p] {
                seen[p] = true
                touches[p] = append(touches[p], Touch{Height: height, TxIndex: uint16(txIndex)})
            }
        }
        for _, o := range spent[txIndex] {
            if p := string(Prefix(o.Script)); len(o.Script) > 0 && !seen[p] {
                seen[p] = true
                touches[p] = append(touches[p], Touch{Height: height, TxIndex: uint16(txIndex)})
            }
        }
    }
}

// parseBlockOutputs reads a serialized block (80-byte header, then the
// transaction count, then each transaction) and returns each transaction's
// outputs — script and amount — indexed by the transaction's position in the
// block. It skips
// everything else — inputs, witness data, locktime — since the spending side
// comes from parseSpentOutputs instead.
func parseBlockOutputs(raw []byte) ([][]Payment, bool) {
    var r = &reader{buf: raw}
    r.skip(80) // block header
    var txCount, ok = r.varInt()
    if !ok { return nil, false }
    var result = make([][]Payment, txCount)
    for i := uint64(0); i < txCount; i++ {
        var scripts, txOK = skipTxKeepOutputs(r)
        if !txOK { return nil, false }
        result[i] = scripts
    }
    if r.bad { return nil, false }
    return result, true
}

func skipTxKeepOutputs(r *reader) ([]Payment, bool) {
    r.skip(4) // version
    var inCount, ok = r.varInt()
    if !ok { return nil, false }
    var segwit bool
    if inCount == 0 { // segwit marker; the real input count follows the flag byte
        segwit = true
        r.skip(1)
        inCount, ok = r.varInt()
        if !ok { return nil, false }
    }
    for i := uint64(0); i < inCount; i++ {
        r.skip(36) // prevout hash + index
        var scriptLen, lenOK = r.varInt()
        if !lenOK { return nil, false }
        r.skip(int(scriptLen))
        r.skip(4) // sequence
    }
    var outCount, outOK = r.varInt()
    if !outOK { return nil, false }
    var scripts = make([]Payment, 0, outCount)
    for i := uint64(0); i < outCount; i++ {
        var sat, satOK = r.value()
        if !satOK { return nil, false }
        var scriptLen, lenOK = r.varInt()
        if !lenOK { return nil, false }
        var script, scriptOK = r.bytes(int(scriptLen))
        if !scriptOK { return nil, false }
        scripts = append(scripts, Payment{Script: script, Sat: sat})
    }
    if segwit {
        for i := uint64(0); i < inCount; i++ {
            var itemCount, itemOK = r.varInt()
            if !itemOK { return nil, false }
            for j := uint64(0); j < itemCount; j++ {
                var itemLen, ilOK = r.varInt()
                if !ilOK { return nil, false }
                r.skip(int(itemLen))
            }
        }
    }
    r.skip(4) // locktime
    return scripts, true
}

// parseSpentOutputs reads Core's REST spent-outputs format for a block: a
// transaction count, then per transaction a count of spent outputs and that many
// serialized TxOuts (8-byte value + script) — the prevout each real input
// consumes, which is the only place a spend's script and amount are written
// down at all. Coinbase transactions report zero
// spent outputs (they have no real inputs), which is why this, like
// parseBlockOutputs, is indexed by transaction position rather than skipping the
// coinbase specially.
func parseSpentOutputs(raw []byte) ([][]Payment, bool) {
    var r = &reader{buf: raw}
    var txCount, ok = r.varInt()
    if !ok { return nil, false }
    var result = make([][]Payment, txCount)
    for i := uint64(0); i < txCount; i++ {
        var outCount, outOK = r.varInt()
        if !outOK { return nil, false }
        var scripts = make([]Payment, 0, outCount)
        for j := uint64(0); j < outCount; j++ {
            var sat, satOK = r.value()
            if !satOK { return nil, false }
            var scriptLen, lenOK = r.varInt()
            if !lenOK { return nil, false }
            var script, scriptOK = r.bytes(int(scriptLen))
            if !scriptOK { return nil, false }
            scripts = append(scripts, Payment{Script: script, Sat: sat})
        }
        result[i] = scripts
    }
    if r.bad { return nil, false }
    return result, true
}

// reader and its varInt are a second, block-scale copy of the same primitives
// zmq.go uses for a single mempool transaction. They are not shared: one walks a
// standalone transaction to find inputs and outputs for live matching, this one
// walks a whole block (and a whole block's spent-outputs blob) to extract
// outputs only — different inputs, different outputs, different call sites.
type reader struct {
    buf []byte
    pos int
    bad bool
}

func (r *reader) skip(n int) {
    if n < 0 || r.pos+n > len(r.buf) { r.bad = true; return }
    r.pos += n
}

func (r *reader) bytes(n int) ([]byte, bool) {
    if n < 0 || r.pos+n > len(r.buf) { r.bad = true; return nil, false }
    var out = r.buf[r.pos : r.pos+n]
    r.pos += n
    return out, true
}

// value reads an output's amount, the 8-byte little-endian satoshi field every
// TxOut carries in front of its script.
func (r *reader) value() (int64, bool) {
    var b, ok = r.bytes(8)
    if !ok { return 0, false }
    return int64(binary.LittleEndian.Uint64(b)), true
}

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

// Scripts returns every distinct scriptPubKey a block touches — those its
// outputs pay and those its inputs spend — using the same parse the indexer
// runs. It is exported so a second pass over the chain can walk addresses
// without a second copy of the block format; tools/addrindex's actbuild is that
// caller.
func Scripts(blk Block) ([][]byte, bool) {
    var outputs, ok1 = parseBlockOutputs(blk.Raw)
    var spent, ok2 = parseSpentOutputs(blk.Spent)
    if !ok1 || !ok2 || len(outputs) != len(spent) { return nil, false }
    var seen = map[string]bool{}
    var out [][]byte
    for _, group := range [][][]Payment{outputs, spent} {
        for _, perTx := range group {
            for _, o := range perTx {
                if len(o.Script) == 0 { continue }
                if seen[string(o.Script)] { continue }
                seen[string(o.Script)] = true
                out = append(out, o.Script)
            }
        }
    }
    return out, true
}

// OutputScripts returns the distinct scriptPubKeys a block's outputs pay to,
// using the same parse the indexer runs.
//
// Outputs only, where Scripts also takes the spending side. That is enough to
// enumerate every address the chain has ever seen: an input can only spend an
// output that was paid earlier, so every script that is ever spent was already
// seen when its funding block was scanned. It is what lets a pass over the raw
// block files skip Core's undo data entirely.
func OutputScripts(raw []byte) ([][]byte, bool) {
    var outputs, ok = parseBlockOutputs(raw)
    if !ok { return nil, false }
    var seen = map[string]bool{}
    var out [][]byte
    for _, perTx := range outputs {
        for _, o := range perTx {
            if len(o.Script) == 0 || seen[string(o.Script)] { continue }
            seen[string(o.Script)] = true
            out = append(out, o.Script)
        }
    }
    return out, true
}

// OutputsByTx returns each transaction's output scripts, indexed by the
// transaction's position in the block. Where OutputScripts flattens a block into
// its distinct scripts, this keeps the transaction boundaries — which is what a
// caller counting *transactions* per address needs, since an address paid twice
// by one transaction was involved in one transaction, not two.
func OutputsByTx(raw []byte) ([][]Payment, bool) { return parseBlockOutputs(raw) }

// Balances returns every change one block makes to a script's balance: each
// output pays its own script, and each spent prevout takes back out of the
// script it was paid to. Nothing else moves value on the chain, so adding these
// up from genesis to a height leaves exactly what every script holds at that
// height — the UTXO set, aggregated by script.
//
// The list is flat, in block order, and deliberately not deduplicated: a script
// paid twice is paid twice, and a caller summing into its own table wants both.
// Nor is an amount ever dropped, so a block's changes sum to the subsidy its
// miner claimed. tools/addrindex's richbuild is the caller.
func Balances(blk Block) ([]Payment, bool) {
    var outputs, ok1 = parseBlockOutputs(blk.Raw)
    var spent, ok2 = parseSpentOutputs(blk.Spent)
    if !ok1 || !ok2 || len(outputs) != len(spent) { return nil, false }
    var n int
    for i := range outputs { n += len(outputs[i]) + len(spent[i]) }
    var out = make([]Payment, 0, n)
    for _, perTx := range outputs { out = append(out, perTx...) }
    for _, perTx := range spent {
        for _, o := range perTx {
            out = append(out, Payment{Script: o.Script, Sat: -o.Sat})
        }
    }
    return out, true
}

// BlockTime is the timestamp in a serialized block's header — the 4-byte
// little-endian field at offset 68, after the version, the previous block's hash
// and the merkle root. It is here rather than at the caller for the reason the
// rest of the format is: one place knows where the bytes are.
func BlockTime(raw []byte) (int64, bool) {
    if len(raw) < 80 { return 0, false }
    return int64(binary.LittleEndian.Uint32(raw[68:72])), true
}
