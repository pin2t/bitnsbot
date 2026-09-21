package addrindex

import "context"
import "fmt"
import "time"
import "bitnsbot/core"
import "bitnsbot/logging"

// Block is what BlockAt reads for one height: when it was mined, and its
// transactions in block order, each with the outputs it pays and the prevouts
// its inputs spend. That is everything a touch needs — which scripts a
// transaction pays, and which it spends from — without a single txid: a touch is
// keyed by (height, tx index in block), not by txid — see addrindex.go. The
// position in Txs is that index.
type Block struct {
    Hash string
    Time int64
    Txs  []Tx
}

// Tx is one transaction's two sides. A coinbase spends nothing, so its Spent is
// empty rather than the transaction being left out, which is what keeps a
// transaction's position in the block its index.
type Tx struct {
    Outputs []Payment
    Spent   []Payment
}

// chunkSize bounds how many blocks are merged into the index per
// transaction, the same reasoning as the miners collector: catching up hundreds
// of thousands of blocks must not build one giant transaction, and a crash
// mid-catch-up should resume from the last flushed chunk, not the beginning. A
// package var so tests shrink it.
var chunkSize int = 1000

// Build catches the index up to the current tip and returns. tools/addrindex
// calls it directly, so a command-line build and the bot's own catch-up advance
// the same cursor through the same chunking; the bot itself goes through Update,
// whose tip its caller has already fetched.
func Build() error {
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var count, err = core.GetBlockCount(ctx)
    if err != nil { return err }
    return Update(int64(count))
}

// Update advances the index from its cursor to tip, in chunks, and returns. The
// bot's one index catch-up goroutine calls it — blocks, miner statistics,
// addrindex, addrstat, addrbal, in that order — after fetching the tip once, so
// a genesis-to-tip build on a fresh index is a multi-hour, one-time cost paid
// the same way every other index pays its own catch-up.
func Update(tip int64) error {
    var ctx, cancel = context.WithTimeout(context.Background(), 6*time.Hour)
    defer cancel()
    var height, ok = Cursor()
    var from int
    if ok { from = height + 1 }
    for from <= int(tip) {
        var to = from + chunkSize - 1
        if to > int(tip) { to = int(tip) }
        var touches = map[string][]Touch{}
        for h := from; h <= to; h++ {
            var blk, berr = BlockAt(ctx, h)
            if berr != nil { return berr }
            indexBlock(touches, uint32(h), blk)
        }
        if err := merge(touches, to); err != nil { return err }
        var bm = fmt.Sprintf("blocks %d..%d", from, to)
        if from == to { bm = fmt.Sprintf("block %d", from) }
        logging.Info("addrindex: built from " + bm)
        from = to + 1
    }
    return nil
}

// Payment is a scriptPubKey and the satoshi paid to it — one output of a
// transaction, or one prevout an input spends. The index needs only the script;
// a balance needs the amount too, so both are kept and each caller takes what it
// uses.
type Payment struct {
    Script []byte
    Sat    int64
}

// indexBlock extracts every touch in one block into the running chunk map: every
// output's script is a funding touch, every spent prevout's script a spending
// touch. Both are gathered per transaction and deduplicated there, so a
// transaction touching the same address more than once — two outputs to one
// address, or an address appearing in both an input and an output of the same
// transaction — is recorded once.
func indexBlock(touches map[string][]Touch, height uint32, blk Block) {
    for txIndex, tx := range blk.Txs {
        var seen = map[string]bool{}
        for _, side := range [][]Payment{tx.Outputs, tx.Spent} {
            for _, o := range side {
                if p := string(Prefix(o.Script)); len(o.Script) > 0 && !seen[p] {
                    seen[p] = true
                    touches[p] = append(touches[p], Touch{Height: height, TxIndex: uint16(txIndex)})
                }
            }
        }
    }
}

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
func Balances(blk Block) []Payment {
    var n int
    for _, tx := range blk.Txs { n += len(tx.Outputs) + len(tx.Spent) }
    var out = make([]Payment, 0, n)
    for _, tx := range blk.Txs { out = append(out, tx.Outputs...) }
    for _, tx := range blk.Txs {
        for _, o := range tx.Spent {
            out = append(out, Payment{Script: o.Script, Sat: -o.Sat})
        }
    }
    return out
}
