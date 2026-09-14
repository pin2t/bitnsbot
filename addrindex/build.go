package addrindex

import "context"
import "fmt"
import "time"
import "bitnsbot/logging"
import "bitnsbot/signals"

// Block is what Blockchain hands over for one height: when it was mined, and its
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

// Blockchain supplies chain data to the backfill, mirroring how the miners
// package takes its chain data through an interface because it can't reach Core
// directly either. RPCBlockchain is the one implementation outside the tests.
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

// backfillInterval is the longest a pass waits once the index is at the tip. A
// block notification cuts the wait short (see the signals package), so this is
// the floor under which nothing is missed rather than the usual cadence.
var backfillInterval = 2 * time.Minute

// StartBackfill walks the chain from the index's cursor to the tip, in chunks,
// and keeps polling for new blocks afterward. It is meant to run for as long as
// the bot does; building genesis-to-tip on a fresh index is a multi-hour, one-
// time cost paid the same way the miners collector pays its own catch-up.
func StartBackfill(src Blockchain) {
    go func() {
        var wake = signals.Subscribe(signals.Block)
        for {
            if err := Build(src); err != nil {
                logging.Warn("addrindex: %v", err)
            }
            select {
            case <-time.After(backfillInterval):
            case <-wake:
            }
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
        var bm = fmt.Sprintf("blocks %d..%d", from, to)
        if from == to { bm = fmt.Sprintf("block %d", from) }
        logging.Info("addrindex: built %s (tip %d)", bm, tip)
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
