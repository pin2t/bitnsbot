package addrindex

import "context"
import "encoding/hex"
import "fmt"
import "math"
import "time"
import "bitnsbot/core"

// blockTimeout bounds one block's two calls. Nothing else does — the core
// client sets no timeout of its own and a build runs under a context hours long
// — so without it a node that stops answering would hold a pass for as long as
// it stayed silent. A minute is far past what verbosity 3 of a full block takes.
var blockTimeout = time.Minute

// output is an amount and a script the way getblock reports them, which is the
// same for an output and for the prevout an input spends.
type output struct {
    Value        float64 `json:"value"`
    ScriptPubKey struct {
        Hex string `json:"hex"`
    } `json:"scriptPubKey"`
}

// BlockAt reads one block from Bitcoin Core's JSON-RPC, the same interface
// everything else in the bot talks to, so a node needs nothing enabled beyond
// its RPC server and ZMQ.
//
// One getblock at verbosity 3 carries all of it: every output's script and
// amount, and every input's prevout with the same two — which Core reads from
// its undo data, the only place a spend's script is written down. The reply is
// read straight into a Block; nothing is serialized, and nothing but the fields a
// scan uses is kept. It goes through core.Call rather than GetBlockVerbose,
// whose transactions decode every field and whose cache would hold a hundred of
// these.
//
// The price is JSON. Verbosity 3 of a full mainnet block is ~13.7 MB where the
// same block and its spent outputs serialized are ~1.95 MB, and Core has to write
// every field of it; a full-chain pass is correspondingly slower than one over
// the binary REST endpoints this replaced.
//
// It lives here rather than in core because this is how the index is built: the
// bot and tools/addrindex both drive the backfill, and addrstat and the tool's
// rankings read the same blocks.
//
// Core reports an amount as a BTC number, and a float's nearest value to one
// is often a hair under it — 0.29 BTC is 28999999.999999996 satoshi — so the
// satoshi are rounded, never truncated.
//
// a coinbase's input has no prevout, so it spends nothing
func BlockAt(ctx context.Context, height int) (Block, error) {
    var bctx, cancel = context.WithTimeout(ctx, blockTimeout)
    defer cancel()
    var hash, err = core.GetBlockHash(bctx, int64(height))
    if err != nil { return Block{}, err }
    var reply struct {
        Time int64 `json:"time"`
        Tx   []struct {
            Vin []struct {
                PrevOut *output `json:"prevout"`
            } `json:"vin"`
            Vout []output `json:"vout"`
        } `json:"tx"`
    }
    if err := core.Call(bctx, "getblock", []interface{}{hash, 3}, &reply); err != nil { return Block{}, err }
    var payment = func(o output) (Payment, error) {
        var script, err = hex.DecodeString(o.ScriptPubKey.Hex)
        if err != nil { return Payment{}, fmt.Errorf("block %s: malformed script %q", hash, o.ScriptPubKey.Hex) }
        return Payment{Script: script, Sat: int64(math.Round(o.Value * 1e8))}, nil
    }
    var blk = Block{Hash: hash, Time: reply.Time, Txs: make([]Tx, len(reply.Tx))}
    for i, t := range reply.Tx {
        var tx = &blk.Txs[i]
        tx.Outputs = make([]Payment, 0, len(t.Vout))
        for _, o := range t.Vout {
            var p, err = payment(o)
            if err != nil { return Block{}, err }
            tx.Outputs = append(tx.Outputs, p)
        }
        for _, in := range t.Vin {
            if in.PrevOut == nil { continue }
            var p, err = payment(*in.PrevOut)
            if err != nil { return Block{}, err }
            tx.Spent = append(tx.Spent, p)
        }
    }
    return blk, nil
}
