package main

import "context"
import "encoding/json"
import "fmt"
import "strconv"
import "strings"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/app"
import "bitnsbot/logging"
import "bitnsbot/miners"
import "bitnsbot/cursors"

var blocksBucket = []byte("blocks")

// oldBlocksBucket is where the cache used to live. blockInit moves whatever is
// still in it across and drops it.
var oldBlocksBucket = []byte("blocks-stat")

// blocksMigrateBatch is how many records one migration transaction moves. The
// cache is chain-sized — a real one holds nearly a million blocks — so a single
// transaction would hold every one of them before it committed, and would give
// bbolt no chance to reuse the pages the old bucket is freeing. A package var so
// tests can shrink it.
var blocksMigrateBatch = 10000

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
    FeeMin     int64    `json:"minFee"`
    FeeAvg     int64    `json:"avgFee"`
    FeeMax     int64    `json:"maxFee"`
    TxSizeMin  int32    `json:"txSizeMin"`
    TxSizeAvg  int32    `json:"txSizeAvg"`
    TxSizeMax  int32    `json:"txSizeMax"`
    Reward     int64    `json:"reward"`
    Total      int64    `json:"total"`
    Difficulty float64  `json:"difficulty"`
}

// blockInit creates the blocks bucket inside the shared bbolt file, ensures the
// shared cursors bucket the backfill keeps its place in, and carries across
// anything left in the bucket the cache used to live in. Called once by openDB
// before any goroutine reads or writes them.
func blockInit(handle *bbolt.DB) error {
    if err := cursors.Init(handle); err != nil { return err }
    var err = handle.Update(func(tx *bbolt.Tx) error {
        var _, berr = tx.CreateBucketIfNotExists(blocksBucket)
        return berr
    })
    if err != nil { return err }
    return migrateBlocks(handle)
}

// migrateBlocks moves the cache out of blocks-stat, the bucket it used to live
// in, and then drops that bucket. It is a no-op on every start after the first,
// the bucket being gone.
//
// A record moves by key, a batch at a time, rather than the whole bucket moving
// in one transaction: the cache is chain-sized — a mainnet one holds nearly a
// million blocks — so one transaction would be a commit of the entire cache, and
// the pages the old bucket frees only become available to the new one once the
// transaction that freed them is closed. Moving by key is also what makes an
// interrupted run recoverable: whatever is left is still in blocks-stat, and the
// next start carries on from there.
func migrateBlocks(handle *bbolt.DB) error {
    var began = time.Now()
    var moved int
    for {
        var done bool
        var err = handle.Update(func(tx *bbolt.Tx) error {
            var old = tx.Bucket(oldBlocksBucket)
            if old == nil {
                done = true
                return nil
            }
            // The bytes a cursor yields belong to the transaction and the puts
            // below may move the pages holding them, so the batch is collected
            // into copies before anything is written.
            type record struct{ key, value []byte }
            var batch []record
            var c = old.Cursor()
            for k, v := c.First(); k != nil && len(batch) < blocksMigrateBatch; k, v = c.Next() {
                batch = append(batch, record{append([]byte(nil), k...), append([]byte(nil), v...)})
            }
            if len(batch) == 0 {
                done = true
                return tx.DeleteBucket(oldBlocksBucket)
            }
            var b = tx.Bucket(blocksBucket)
            for _, r := range batch {
                if err := b.Put(r.key, r.value); err != nil { return err }
                if err := old.Delete(r.key); err != nil { return err }
            }
            moved += len(batch)
            return nil
        })
        if err != nil { return err }
        if done { break }
    }
    if moved > 0 {
        logging.Status("blocks: moved %d records out of blocks-stat in %s", moved, time.Since(began).Round(time.Millisecond))
    }
    return nil
}

func storeBlock(bi *blockInfo) error {
    if db == nil { return nil }
    logging.Db("blocks: store %d", bi.Height)
    var data, err = json.Marshal(bi)
    if err != nil { return err }
    return db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket(blocksBucket).Put(itob(uint64(bi.Height)), data)
    })
}

func loadBlock(height int64) (*blockInfo, bool) {
    if db == nil { return nil, false }
    logging.Db("blocks: load %d", height)
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
// circulatingSupply is the total mined at this height, in satoshi, summed one
// halving epoch at a time (at most 33 iterations rather than one per block).
//
// It is the issuance schedule, not a UTXO-set total: coins burned or lost, and
// the handful of blocks whose miners under-claimed their subsidy, are all still
// counted. `gettxoutsetinfo` would be exact but walks the whole UTXO set, which
// takes minutes — far too slow for a card that refreshes every ten.
func circulatingSupply(height int64) int64 {
    var total int64
    for epoch := int64(0); epoch < 64; epoch++ {
        var start = epoch * 210000
        if start > height { break }
        var end = start + 210000 - 1
        if end > height { end = height }
        total += (end - start + 1) * subsidy(start)
    }
    return total
}

func subsidy(height int64) int64 {
    var halvings = height / 210000
    if halvings >= 64 { return 0 }
    return int64(5000000000) >> uint(halvings)
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
    var coinbaseOut int64
    var addrs []string
    for _, v := range coinbase.Vout {
        coinbaseOut += toSat(v.Value)
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
        logging.Warn("blocks: process %s: %v", short(hash), err)
        return
    }
    if err := storeBlock(bi); err != nil {
        logging.Err("blocks: store %d: %v", bi.Height, err)
        return
    }
    logging.Info("blocks: processed %d mined by %s", bi.Height, bi.Miner)
    // Notify only once the block is actually stored: the Blocks tab reads the
    // cache, so announcing earlier would have the page re-fetch the old list.
    app.Notify("blocks")
}

// startBlockCache runs a goroutine that catches up from the last processed block
// to the current tip every blockCacheInterval, storing each block's stats in the
// blocks bucket. New blocks also arrive over ZMQ (see zmq.go), so the
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
        logging.Warn("blocks: %v", err)
        return
    }
    var cursor, haveCursor = cursors.Get(cursors.Blocks)
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
                logging.Warn("blocks: block %d hash: %v — retrying next run", h, herr)
                return
            }
            bctx, bcancel = context.WithTimeout(context.Background(), 60*time.Second)
            var bi, cerr = computeBlockInfo(bctx, hash)
            bcancel()
            if cerr != nil {
                logging.Warn("blocks: block %d: %v — retrying next run", h, herr)
                return
            }
            infos = append(infos, bi)
        }
        if err := flushBlocks(infos, to); err != nil {
            logging.Err("blocks: flush %v", err)
            return
        }
        if from < tip { time.Sleep(1 * time.Minute) }
        from = to + 1
    }
    if from-1 > began {
        logging.Info("blocks: processed %d blocks, up to %d", from-1-began, from-1)
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
        return cursors.Set(tx, cursors.Blocks, cursor)
    })
}

// formatBlock renders a cached block record as the /info block reply.
// blockPairs builds the label/value lines a block is described by. Shared with
// the Mini App's block details page, so the two cannot drift apart. A pair with
// an empty value is a heading ("Fees", "Tx sizes"), not a field.
func blockPairs(bi *blockInfo, lang string) [][2]string {
    var difficulty = metric(bi.Difficulty, 2)
    var pairs = [][2]string{
        {i18nl(lang).String("Hash"), short(bi.Hash)},
        {i18nl(lang).String("Time"), when(bi.Time, lang)},
        {i18nl(lang).String("Size"), humSize(int64(bi.Size), 2, lang)},
        {i18nl(lang).String("Transactions"), strconv.Itoa(bi.NumTx)},
        {i18nl(lang).String("Miner"), bi.Miner},
        {i18nl(lang).String("Difficulty"), difficulty},
    }
    switch {
    case !bi.FeesOK:     pairs = append(pairs, [2]string{i18nl(lang).String("Fees"), i18nl(lang).String("unavailable")})
    case bi.NumTx <= 1:  pairs = append(pairs, [2]string{i18nl(lang).String("Fees"), i18nl(lang).String("none (coinbase only)")})
    default:
        var feeLine = func (fee int64, sz int32) string {
            return group(fee) + " " + i18nl(lang).String("sats") +
                " (" + strings.TrimSuffix(strconv.FormatFloat(float64(fee) / float64(sz), 'f', 1, 64), ".0") + " " + i18nl(lang).String("sat/vB") + ")"
        }
        pairs = append(pairs,
            [2]string{i18nl(lang).String("Fees"), ""},
            [2]string{i18nl(lang).String("lowest"), feeLine(bi.FeeMin, bi.TxSizeMin)},
            [2]string{i18nl(lang).String("average"), feeLine(bi.FeeAvg, bi.TxSizeAvg)},
            [2]string{i18nl(lang).String("highest"), feeLine(bi.FeeMax, bi.TxSizeMax)},
        )
    }
    pairs = append(pairs,
        [2]string{i18nl(lang).String("Tx sizes"), ""},
        [2]string{i18nl(lang).String("minimum"), group(int64(bi.TxSizeMin)) + " " + i18nl(lang).String("B")},
        // a size, not a fee: Russian declines the two differently, so this is its
        // own key rather than the "average" the fee line above uses
        [2]string{i18nl(lang).String("average-tx"), group(int64(bi.TxSizeAvg)) + " " + i18nl(lang).String("B")},
        [2]string{i18nl(lang).String("maximum"), group(int64(bi.TxSizeMax)) + " " + i18nl(lang).String("B")},
        [2]string{i18nl(lang).String("Reward"), amountLine(bi.Reward, time.Unix(bi.Time, 0), false, lang)},
        [2]string{i18nl(lang).String("Reward + fees"), amountLine(bi.Total, time.Unix(bi.Time, 0), false, lang)},
    )
    return pairs
}

func formatBlock(bi *blockInfo, lang string) string {
    return i18nl(lang).Sprintf("Block #%d\n\n<pre>%s</pre>", bi.Height, joinAlign(blockPairs(bi, lang)))
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
    var total int64
    var addrs []string
    for _, v := range cb.Vout {
        total += toSat(v.Value)
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
