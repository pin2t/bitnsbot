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

var blocksBucket = []byte("blocks-stat")
var blocksCursorBucket = []byte("blocks-cursor")

// blockCacheInterval is how often the collector catches up from the last
// processed block to the chain tip. A package var so tests can shrink it.
var blockCacheInterval = 10 * time.Minute

// blocksChunkSize is how many blocks are collected in memory before a single
// database flush, so the collector writes the cursor only once per chunk.
var blocksChunkSize int64 = 1000

type blockInfo struct {
    Height     int64    `json:"height"`
    Hash       string   `json:"hash"`
    Time       int64    `json:"timestamp"`
    Size       int32    `json:"size"`
    NumTx      int      `json:"txCount"`
    Miner      string   `json:"miner"`
    FeesOK     bool     `json:"feesOk"`
    FeeMin     float64  `json:"minFee"`
    FeeAvg     float64  `json:"avgFee"`
    FeeMax     float64  `json:"maxFee"`
    TxSizeMin  int32    `json:"txSizeMin"`
    TxSizeAvg  int32    `json:"txSizeAvg"`
    TxSizeMax  int32    `json:"txSizeMax"`
    Reward     float64  `json:"reward"`
    Total      float64  `json:"total"`
    Difficulty float64  `json:"difficulty"`
}

// blockInit creates the blocks-stat and blocks-cursor buckets inside the shared
// bbolt file. Called once by openDB before any goroutine reads or writes them.
func blockInit(handle *bbolt.DB) error {
    return handle.Update(func(tx *bbolt.Tx) error {
        for _, name := range [][]byte{blocksBucket, blocksCursorBucket} {
            if _, err := tx.CreateBucketIfNotExists(name); err != nil { return err }
        }
        return nil
    })
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

// processBlock computes and stores a block by hash — used by the blockconnected
// notification, which carries the new tip's hash. Runs off core's read-loop
// goroutine (spawned by the handler) since computeBlockInfo calls back into core.
func processBlock(hash string) {
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

// startBlockCache runs a goroutine that catches up from the last processed block
// to the current tip every blockCacheInterval, storing each block's stats in the
// blocks-stat bucket. New blocks also arrive over ZMQ (see zmq.go), so the
// interval is only a safety net — the typical case is a no-op.
func startBlockCache() {
    go func() {
        collectBlocks()
        var t = time.NewTicker(blockCacheInterval)
        defer t.Stop()
        for range t.C {
            collectBlocks()
        }
    }()
}

func collectBlocks() {
    if core == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()
    var tip, err = core.getBlockCount(ctx)
    if err != nil {
        logging.Warn("block cache: tip: %v", err)
        return
    }
    // read the last processed height from the blocks-cursor bucket
    var cursor int64
    var haveCursor bool
    if db != nil {
        db.View(func(tx *bbolt.Tx) error {
            if v := tx.Bucket(blocksCursorBucket).Get([]byte("cursor")); v != nil {
                var e error
                cursor, e = strconv.ParseInt(string(v), 10, 64)
                if e == nil { haveCursor = true }
            }
            return nil
        })
    }
    var from int64
    if !haveCursor {
        // No cursor yet: rescan from genesis.
        from = 0
    } else {
        from = cursor + 1
    }
    var began = from - 1
    for from <= tip {
        var to = from + blocksChunkSize - 1
        if to > tip { to = tip }
        var infos []*blockInfo
        for h := from; h <= to; h++ {
            var bctx, bcancel = context.WithTimeout(context.Background(), 60*time.Second)
            var hash, herr = core.getBlockHash(bctx, h)
            bcancel()
            if herr != nil {
                logging.Warn("block cache: block %d hash: %v — retrying next run", h, herr)
                return
            }
            bctx, bcancel = context.WithTimeout(context.Background(), 60*time.Second)
            var bi, cerr = computeBlockInfo(bctx, hash)
            bcancel()
            if cerr != nil {
                logging.Warn("block cache: block %d: %v — retrying next run", h, cerr)
                return
            }
            infos = append(infos, bi)
        }
        if err := flushBlocks(infos, to); err != nil {
            logging.Err("block cache: flush: %v", err)
            return
        }
        if from < tip { time.Sleep(1 * time.Minute) }
        from = to + 1
    }
    if from-1 > began {
        logging.Info("block cache: processed %d blocks, up to %d", from-1-began, from-1)
    }
}

// flushBlocks stores a chunk of block info and advances the cursor in one
// transaction. On error the cursor does not move, so the next run retries the
// whole chunk.
func flushBlocks(bis []*blockInfo, cursor int64) error {
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(blocksBucket)
        for _, bi := range bis {
            var data, err = json.Marshal(bi)
            if err != nil { return err }
            if err := b.Put(itob(uint64(bi.Height)), data); err != nil { return err }
        }
        return tx.Bucket(blocksCursorBucket).Put(
            []byte("cursor"), []byte(strconv.FormatInt(cursor, 10)))
    })
}

// formatBlock renders a cached block record as the /info block reply.
func formatBlock(bi *blockInfo, chat int64) string {
    var difficulty = metric(bi.Difficulty, 2)
    var pairs = [][2]string{
        {i18n(chat).String("Hash"), short(bi.Hash)},
        {i18n(chat).String("Time"), when(bi.Time, chat)},
        {i18n(chat).String("Size"), humSize(int64(bi.Size))},
        {i18n(chat).String("Transactions"), strconv.Itoa(bi.NumTx)},
        {i18n(chat).String("Miner"), bi.Miner},
        {i18n(chat).String("Difficulty"), difficulty},
    }
    switch {
    case !bi.FeesOK:
        pairs = append(pairs, [2]string{i18n(chat).String("Fees"), i18n(chat).String("unavailable")})
    case bi.NumTx <= 1:
        pairs = append(pairs, [2]string{i18n(chat).String("Fees"), i18n(chat).String("none (coinbase only)")})
    default:
        var feeLine = func (fee float64, sz int32) string {
            return sats(fee) + i18n(chat).String(" sats") + " (" + strings.TrimSuffix(strconv.FormatFloat(math.Round(fee*1e8) / float64(sz), 'f', 1, 64), ".0") + i18n(chat).String(" sat/vB)")
        }
        pairs = append(pairs,
            [2]string{i18n(chat).String("Fees"), ""},
            [2]string{i18n(chat).String("lowest"), feeLine(bi.FeeMin, bi.TxSizeMin)},
            [2]string{i18n(chat).String("average"), feeLine(bi.FeeAvg, bi.TxSizeAvg)},
            [2]string{i18n(chat).String("highest"), feeLine(bi.FeeMax, bi.TxSizeMax)},
        )
    }
    pairs = append(pairs,
        [2]string{i18n(chat).String("Tx sizes"), ""},
        [2]string{i18n(chat).String("minimum"), group(int64(bi.TxSizeMin)) + " " + i18n(chat).String("B")},
        [2]string{i18n(chat).String("average"), group(int64(bi.TxSizeAvg)) + " " + i18n(chat).String("B")},
        [2]string{i18n(chat).String("maximum"), group(int64(bi.TxSizeMax)) + " " + i18n(chat).String("B")},
        [2]string{i18n(chat).String("Reward"), amountLine(bi.Reward, time.Unix(bi.Time, 0), false)},
        [2]string{i18n(chat).String("Reward + fees"), amountLine(bi.Reward, time.Unix(bi.Time, 0), false)},
    )
    return i18n(chat).Sprintf("Block #%d\n\n<pre>%s</pre>", bi.Height, joinAlign(pairs))
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
