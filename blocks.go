package main

import "context"
import "encoding/json"
import "fmt"
import "strconv"
import "strings"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"
import "bitnsbot/miners"
import "math"

var blocksBucket = []byte("blocks")

// blockBackfillDepth is how many recent blocks the startup goroutine caches
// (skipping any already cached). Newer blocks arrive via the blockconnected
// notification. A package var so tests/verification can shrink it — each block
// costs a getblock plus one prevout fetch per input, so it bounds startup load.
var blockBackfillDepth int64 = 50

type blockInfo struct {
    Height     int64
    Hash       string
    Time       int64
    Size       int32
    NumTx      int
    Miner      string
    FeesOK     bool
    FeeMin     float64
    FeeAvg     float64
    FeeMax     float64
    TxSizeMin  int32
    TxSizeAvg  int32
    TxSizeMax  int32
    Reward     float64
    Total      float64
    Difficulty float64
}

func storeBlock(bi *blockInfo) error {
    if db == nil { return nil }
    logging.Db("store block %d", bi.Height)
    var data, err = json.Marshal(bi)
    if err != nil { return err }
    return db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket(blocksBucket).Put(itob(uint64(bi.Height)), data)
    })
}

func loadBlock(height int64) (*blockInfo, bool) {
    if db == nil { return nil, false }
    logging.Db("load block %d", height)
    var bi blockInfo
    var found bool
    db.View(func(tx *bbolt.Tx) error {
        var v = tx.Bucket(blocksBucket).Get(itob(uint64(height)))
        if v != nil && json.Unmarshal(v, &bi) == nil {
            found = true
        }
        return nil
    })
    if !found { return nil, false }
    return &bi, true
}

// subsidy returns the block reward in BTC for a height from the halving schedule
// — 50 BTC, halving every 210000 blocks.
func subsidy(height int64) float64 {
    var halvings = height / 210000
    if halvings >= 64 { return 0 }
    return float64(int64(5000000000)>>uint(halvings)) / 1e8
}

// computeBlockInfo builds the cached record from core: general fields from
// getblock (verbosity 2), per-tx fee stats from blockFees (which fetches
// prevouts), the transaction-size distribution, the block reward from the
// halving schedule, and the miner attributed from the coinbase. Reward and total
// (reward + fees = coinbase output) are always available; the fee min/avg/max
// need the prevout fetches, so FeesOK records whether they succeeded.
func computeBlockInfo(ctx context.Context, hash string) (*blockInfo, error) {
    var blk, err = core.getBlockVerbose(ctx, hash)
    if err != nil { return nil, err }
    if len(blk.Tx) == 0 { return nil, fmt.Errorf("block %s has no transactions", short(hash)) }
    var szMin, szMax = blk.Tx[0].Size, blk.Tx[0].Size
    var szSum int64
    for _, t := range blk.Tx {
        if t.Size < szMin { szMin = t.Size }
        if t.Size > szMax { szMax = t.Size }
        szSum += int64(t.Size)
    }
    var low, avg, high, _ = feeStats(blk.Tx)
    var coinbase = blk.Tx[0]
    var coinbaseOut float64
    var addrs []string
    for _, v := range coinbase.Vout {
        coinbaseOut += v.Value
        if v.ScriptPubKey.Address != "" { addrs = append(addrs, v.ScriptPubKey.Address) }
    }
    var script string
    if len(coinbase.Vin) > 0 { script = coinbase.Vin[0].Coinbase }
    var miner = miners.Attribute(addrs, script)
    if miner == "" { miner = "Unknown" }
    return &blockInfo{
        Height: blk.Height, Hash: blk.Hash, Time: blk.Time, Size: blk.Size,
        NumTx: len(blk.Tx), Miner: miner,
        FeesOK: true, FeeMin: low, FeeAvg: avg, FeeMax: high,
        TxSizeMin: szMin, TxSizeAvg: int32(szSum / int64(len(blk.Tx))), TxSizeMax: szMax,
        Reward: subsidy(blk.Height), Total: coinbaseOut, Difficulty: blk.Difficulty,
    }, nil
}

// cacheBlockHeight computes and stores a block by height (backfill / on-demand).
func cacheBlockHeight(ctx context.Context, height int64) error {
    var hash, err = core.getBlockHash(ctx, height)
    if err != nil { return err }
    var bi, ciErr = computeBlockInfo(ctx, hash)
    if ciErr != nil { return ciErr }
    return storeBlock(bi)
}

// cacheBlockHash computes and stores a block by hash — used by the blockconnected
// notification, which carries the new tip's hash. Runs off core's read-loop
// goroutine (spawned by the handler) since computeBlockInfo calls back into core.
func cacheBlockHash(hash string) {
    if core == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    var bi, err = computeBlockInfo(ctx, hash)
    if err != nil {
        logging.Warn("cache block %s: %v", short(hash), err)
        return
    }
    if err := storeBlock(bi); err != nil {
        logging.Err("store block %d: %v", bi.Height, err)
        return
    }
    logging.Info("cached block %d mined by %s", bi.Height, bi.Miner)
}

// startBlockCache backfills the most recent blocks into the cache. New blocks
// arrive over ZMQ (see zmq.go) rather than through a per-connection
// subscription, so unlike the btcd path there is nothing to subscribe to here.
func startBlockCache() {
    go func() {
        if core == nil { return }
        var tctx, tcancel = context.WithTimeout(context.Background(), 15*time.Second)
        var tip, err = core.getBlockCount(tctx)
        tcancel()
        if err != nil { logging.Warn("block cache: get tip: %v", err); return }
        for h := tip; h > tip-blockBackfillDepth && h >= 0; h-- {
            if _, ok := loadBlock(h); ok { continue }
            var bctx, bcancel = context.WithTimeout(context.Background(), 60*time.Second)
            if err := cacheBlockHeight(bctx, h); err != nil {
                logging.Warn("backfill block %d: %v", h, err)
            }
            bcancel()
        }
        logging.Info("block cache backfill complete (%d blocks deep)", blockBackfillDepth)
    }()
}

// formatBlock renders a cached block record as the /info block reply.
func formatBlock(bi *blockInfo) string {
    var difficulty = metric(bi.Difficulty, 2)
    var pairs = [][2]string{
        {"Hash", short(bi.Hash)},
        {"Time", when(bi.Time)},
        {"Size", metric(float64(bi.Size), 2)},
        {"Transactions", strconv.Itoa(bi.NumTx)},
        {"Miner", bi.Miner},
        {"Difficulty", difficulty},
    }
    switch {
    case !bi.FeesOK:
        pairs = append(pairs, [2]string{"Fees", "unavailable"})
    case bi.NumTx <= 1:
        pairs = append(pairs, [2]string{"Fees", "none (coinbase only)"})
    default:
        var feeLine = func (fee float64, sz int32) string {
            return sats(fee) + " sats (" + strings.TrimSuffix(strconv.FormatFloat(math.Round(fee*1e8) / float64(sz), 'f', 1, 64), ".0") + " sat/vB)"
        }
        pairs = append(pairs,
            [2]string{"Fees", ""},
            [2]string{"lowest", feeLine(bi.FeeMin, bi.TxSizeMin)},
            [2]string{"average", feeLine(bi.FeeAvg, bi.TxSizeAvg)},
            [2]string{"highest", feeLine(bi.FeeMax, bi.TxSizeMax)},
        )
    }
    pairs = append(pairs,
        [2]string{"Tx sizes", ""},
        [2]string{"minimum", group(int64(bi.TxSizeMin)) + " bytes"},
        [2]string{"average", group(int64(bi.TxSizeAvg)) + " bytes"},
        [2]string{"maximum", group(int64(bi.TxSizeMax)) + " bytes"},
        [2]string{"Reward", amountLine(bi.Reward, time.Unix(bi.Time, 0), false)},
        [2]string{"Reward + fees", amountLine(bi.Reward, time.Unix(bi.Time, 0), false)},
    )
    var pad int
    for _, p := range pairs {
        if len(p[0])+1 > pad { pad = len(p[0]) + 1 }
    }
    var lines []string
    for _, p := range pairs {
        lines = append(lines, fmt.Sprintf("%-*s %s", pad, p[0]+":", p[1]))
    }
    return fmt.Sprintf("Block #%d\n\n<pre>%s</pre>", bi.Height, strings.Join(lines, "\n"))
}

// minerSource adapts the core connection to the miners package's stats collector:
// per block it fetches the header (verbosity 1 → height + difficulty + txids) and
// the coinbase transaction, from which it reads every payout address, the coinbase
// script (which carries the pool tag) and the total output (subsidy + fees); fees
// are that total minus the height's subsidy. All the coinbase addresses are passed
// on (not just the first) because the pool's payout isn't always output 0 — the
// same reason computeBlockInfo collects them all.
type minerSource struct{}

func (minerSource) Tip(ctx context.Context) (int64, error) {
    return core.getBlockCount(ctx)
}

func (minerSource) Block(ctx context.Context, height int64) (miners.Block, error) {
    var hash, err = core.getBlockHash(ctx, height)
    if err != nil { return miners.Block{}, err }
    var blk, berr = core.getBlockTxids(ctx, hash)
    if berr != nil { return miners.Block{}, berr }
    if len(blk.Tx) == 0 { return miners.Block{}, fmt.Errorf("block %d has no transactions", height) }
    var cb, cerr = core.getRawTransaction(ctx, blk.Tx[0])
    if cerr != nil { return miners.Block{}, cerr }
    var total float64
    var addrs []string
    for _, v := range cb.Vout {
        total += v.Value
        if v.ScriptPubKey.Address != "" { addrs = append(addrs, v.ScriptPubKey.Address) }
    }
    var script string
    if len(cb.Vin) > 0 { script = cb.Vin[0].Coinbase }
    return miners.Block{
        CoinbaseAddresses: addrs,
        CoinbaseScript:    script,
        Reward:            total,
        Fees:              total - subsidy(height),
        Difficulty:        blk.Difficulty,
    }, nil
}
