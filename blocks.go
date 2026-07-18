package main

import "context"
import "encoding/json"
import "fmt"
import "strconv"
import "strings"
import "time"

import "go.etcd.io/bbolt"

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
    logDb("store block %d", bi.Height)
    var data, err = json.Marshal(bi)
    if err != nil { return err }
    return db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket(blocksBucket).Put(itob(uint64(bi.Height)), data)
    })
}

func loadBlock(height int64) (*blockInfo, bool) {
    if db == nil { return nil, false }
    logDb("load block %d", height)
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

// computeBlockInfo builds the cached record from btcd: general fields from
// getblock (verbosity 2), per-tx fee stats from blockFees (which fetches
// prevouts), the transaction-size distribution, the block reward from the
// halving schedule, and the miner attributed from the coinbase. Reward and total
// (reward + fees = coinbase output) are always available; the fee min/avg/max
// need the prevout fetches, so FeesOK records whether they succeeded.
func computeBlockInfo(ctx context.Context, hash string) (*blockInfo, error) {
    var blk, err = btcd.getBlockVerbose(ctx, hash)
    if err != nil { return nil, err }
    if len(blk.Tx) == 0 { return nil, fmt.Errorf("block %s has no transactions", short(hash)) }
    var szMin, szMax = blk.Tx[0].Size, blk.Tx[0].Size
    var szSum int64
    for _, t := range blk.Tx {
        if t.Size < szMin { szMin = t.Size }
        if t.Size > szMax { szMax = t.Size }
        szSum += int64(t.Size)
    }
    var low, avg, high, _, feeErr = blockFees(ctx, blk.Tx)
    var coinbase = blk.Tx[0]
    var coinbaseOut float64
    var addrs []string
    for _, v := range coinbase.Vout {
        coinbaseOut += v.Value
        if v.ScriptPubKey.Address != "" { addrs = append(addrs, v.ScriptPubKey.Address) }
        addrs = append(addrs, v.ScriptPubKey.Addresses...)
    }
    var scriptHex string
    if len(coinbase.Vin) > 0 { scriptHex = coinbase.Vin[0].Coinbase }
    return &blockInfo{
        Height: blk.Height, Hash: blk.Hash, Time: blk.Time, Size: blk.Size,
        NumTx: len(blk.Tx), Miner: minerName(scriptHex, addrs),
        FeesOK: feeErr == nil, FeeMin: low, FeeAvg: avg, FeeMax: high,
        TxSizeMin: szMin, TxSizeAvg: int32(szSum / int64(len(blk.Tx))), TxSizeMax: szMax,
        Reward: subsidy(blk.Height), Total: coinbaseOut, Difficulty: blk.Difficulty,
    }, nil
}

// cacheBlockHeight computes and stores a block by height (backfill / on-demand).
func cacheBlockHeight(ctx context.Context, height int64) error {
    var hash, err = btcd.getBlockHash(ctx, height)
    if err != nil { return err }
    var bi, ciErr = computeBlockInfo(ctx, hash)
    if ciErr != nil { return ciErr }
    return storeBlock(bi)
}

// cacheBlockHash computes and stores a block by hash — used by the blockconnected
// notification, which carries the new tip's hash. Runs off btcd's read-loop
// goroutine (spawned by the handler) since computeBlockInfo calls back into btcd.
func cacheBlockHash(hash string) {
    if btcd == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    var bi, err = computeBlockInfo(ctx, hash)
    if err != nil {
        logWarn("cache block %s: %v", short(hash), err)
        return
    }
    if err := storeBlock(bi); err != nil {
        logErr("store block %d: %v", bi.Height, err)
        return
    }
    logInfo("cached block %d mined by %s", bi.Height, bi.Miner)
}

// startBlockCache loads the mining-pool definitions, subscribes to btcd block
// notifications (so new blocks are cached as they arrive, via notifier.Handle),
// and backfills the most recent blocks into the cache.
func startBlockCache() {
    go func() {
        if err := loadPools(); err != nil { logWarn("load mining pools: %v", err) }
        if btcd == nil { return }
        var nctx, ncancel = context.WithTimeout(context.Background(), 15*time.Second)
        if err := btcd.notifyBlocks(nctx); err != nil { logWarn("subscribe to blocks: %v", err) }
        ncancel()
        var tctx, tcancel = context.WithTimeout(context.Background(), 15*time.Second)
        var tip, err = btcd.getBlockCount(tctx)
        tcancel()
        if err != nil { logWarn("block cache: get tip: %v", err); return }
        for h := tip; h > tip-blockBackfillDepth && h >= 0; h-- {
            if _, ok := loadBlock(h); ok { continue }
            var bctx, bcancel = context.WithTimeout(context.Background(), 60*time.Second)
            if err := cacheBlockHeight(bctx, h); err != nil {
                logWarn("backfill block %d: %v", h, err)
            }
            bcancel()
        }
        logInfo("block cache backfill complete (%d blocks deep)", blockBackfillDepth)
    }()
}

// formatBlock renders a cached block record as the /info block reply.
func formatBlock(bi *blockInfo) string {
    var diff = bi.Difficulty
    var unit = ""
    for _, u := range []string{" k", " M", " G", " T", " P", " E"} {
        if diff < 1000 { break }
        diff /= 1000
        unit = u
    }
    var difficulty = strings.TrimRight(strings.TrimRight(strconv.FormatFloat(diff, 'f', 2, 64), "0"), ".") + unit
    var pairs = [][2]string{
        {"Hash", short(bi.Hash)},
        {"Time", when(bi.Time)},
        {"Size", group(int64(bi.Size)) + " bytes"},
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
        pairs = append(pairs,
            [2]string{"Lowest fee", satoshi(bi.FeeMin) + " sats"},
            [2]string{"Average fee", satoshi(bi.FeeAvg) + " sats"},
            [2]string{"Highest fee", satoshi(bi.FeeMax) + " sats"},
        )
    }
    pairs = append(pairs,
        [2]string{"Tx size min", group(int64(bi.TxSizeMin)) + " bytes"},
        [2]string{"Tx size avg", group(int64(bi.TxSizeAvg)) + " bytes"},
        [2]string{"Tx size max", group(int64(bi.TxSizeMax)) + " bytes"},
        [2]string{"Reward", satoshi(bi.Reward) + " sats"},
        [2]string{"Reward + fees", satoshi(bi.Total) + " sats"},
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
