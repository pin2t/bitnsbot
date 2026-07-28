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
            if err := catchUp(src); err != nil {
                logging.Warn("addrindex: %v", err)
            }
            time.Sleep(backfillInterval)
        }
    }()
}

func catchUp(src Blockchain) error {
    var ctx, cancel = context.WithTimeout(context.Background(), 6*time.Hour)
    defer cancel()
    var tip, err = src.Tip(ctx)
    if err != nil { return err }
    var height, ok = LoadCursor()
    var from int
    if ok { from = height + 1 }
    for from <= tip {
        var to = from + chunkSize - 1
        if to > tip { to = tip }
        var touches = map[string][]Touch{}
        for h := from; h <= to; h++ {
            var blk, berr = src.BlockAt(ctx, h)
            if berr != nil {
                // abandon the chunk without merging, same reasoning as the
                // miners collector: advancing the cursor past a block that
                // failed to fetch would drop it from the index for good, and
                // the next pass retries the whole range
                return berr
            }
            indexBlock(touches, uint32(h), blk)
        }
        if err := merge(touches, to); err != nil { return err }
        logging.Info("addrindex: built blocks %d..%d (tip %d)", from, to, tip)
        from = to + 1
    }
    return nil
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
func indexBlockFromParsed(touches map[string][]Touch, height uint32, outputs, spent [][][]byte) {
    for txIndex := range outputs {
        var seen = map[string]bool{}
        for _, s := range outputs[txIndex] {
            if p := string(Prefix(s)); len(s) > 0 && !seen[p] {
                seen[p] = true
                touches[p] = append(touches[p], Touch{Height: height, TxIndex: uint16(txIndex)})
            }
        }
        for _, s := range spent[txIndex] {
            if p := string(Prefix(s)); len(s) > 0 && !seen[p] {
                seen[p] = true
                touches[p] = append(touches[p], Touch{Height: height, TxIndex: uint16(txIndex)})
            }
        }
    }
}

// parseBlockOutputs reads a serialized block (80-byte header, then the
// transaction count, then each transaction) and returns each transaction's
// output scripts, indexed by the transaction's position in the block. It skips
// everything else — inputs, witness data, locktime — since the spending side
// comes from parseSpentOutputs instead.
func parseBlockOutputs(raw []byte) ([][][]byte, bool) {
    var r = &reader{buf: raw}
    r.skip(80) // block header
    var txCount, ok = r.varInt()
    if !ok { return nil, false }
    var result = make([][][]byte, txCount)
    for i := uint64(0); i < txCount; i++ {
        var scripts, txOK = skipTxKeepOutputs(r)
        if !txOK { return nil, false }
        result[i] = scripts
    }
    if r.bad { return nil, false }
    return result, true
}

func skipTxKeepOutputs(r *reader) ([][]byte, bool) {
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
    var scripts = make([][]byte, 0, outCount)
    for i := uint64(0); i < outCount; i++ {
        r.skip(8) // value
        var scriptLen, lenOK = r.varInt()
        if !lenOK { return nil, false }
        var script, scriptOK = r.bytes(int(scriptLen))
        if !scriptOK { return nil, false }
        scripts = append(scripts, script)
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
// serialized TxOuts (8-byte value + script). Coinbase transactions report zero
// spent outputs (they have no real inputs), which is why this, like
// parseBlockOutputs, is indexed by transaction position rather than skipping the
// coinbase specially.
func parseSpentOutputs(raw []byte) ([][][]byte, bool) {
    var r = &reader{buf: raw}
    var txCount, ok = r.varInt()
    if !ok { return nil, false }
    var result = make([][][]byte, txCount)
    for i := uint64(0); i < txCount; i++ {
        var outCount, outOK = r.varInt()
        if !outOK { return nil, false }
        var scripts = make([][]byte, 0, outCount)
        for j := uint64(0); j < outCount; j++ {
            r.skip(8) // value
            var scriptLen, lenOK = r.varInt()
            if !lenOK { return nil, false }
            var script, scriptOK = r.bytes(int(scriptLen))
            if !scriptOK { return nil, false }
            scripts = append(scripts, script)
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
