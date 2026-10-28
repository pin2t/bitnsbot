// Package blocks is the block info cache: one record per height in the blocks
// table, built from Core in the background so a block lookup is a database read
// rather than a getblock in the user's request. Update is the collector that
// fills it, Load and Scan read it back, and Compute builds a record for a block
// the collector has not reached yet.
package blocks

import "context"
import "fmt"
import "math"
import "time"
import "database/sql"
import "bitnsbot/app"
import "bitnsbot/core"
import "bitnsbot/logging"
import "bitnsbot/progress"
import "bitnsbot/miners"

var db *sql.DB

// chunkSize is how many blocks are collected in memory before a single
// database flush.
var chunkSize int64 = 1000

// chunkPause is how long the collector waits between chunks of a long
// catch-up. A package var so tests do not sit through it.
var chunkPause = 1 * time.Minute

// Info is one cached block. Total is the whole coinbase output, reward plus
// fees.
type Info struct {
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

// Init is called once by openDB before any goroutine reads or writes the
// blocks table, and creates it, with the blocks_ts index the miner chart reads a
// period through and the blocks_hash index Rollback finds a disconnected block
// through, when they are missing. A miner stored as the literal "Unknown" by
// an earlier version is made empty, the placeholder being applied at render.
func Init(handle *sql.DB) error {
    db = handle
    if db == nil { return nil }
    for _, s := range []string{
        `create table if not exists blocks (height INTEGER PRIMARY KEY, hash TEXT NOT NULL, ts INTEGER NOT NULL,
            size INTEGER NOT NULL, txs INTEGER NOT NULL, miner TEXT NOT NULL, feesOK INTEGER NOT NULL,
            minFee INTEGER NOT NULL, avgFee INTEGER NOT NULL, maxFee INTEGER NOT NULL,
            txSizeMin INTEGER NOT NULL, txSizeAvg INTEGER NOT NULL, txSizeMax INTEGER NOT NULL,
            reward INTEGER NOT NULL, fees INTEGER NOT NULL, difficulty REAL NOT NULL)`,
        `create index if not exists blocks_ts on blocks (ts)`,
        `create index if not exists blocks_hash on blocks (hash)`,
    } {
        if _, err := db.Exec(s); err != nil { return err }
    }
    db.Exec("update blocks set miner = '' where miner = 'Unknown'")
    return nil
}

// The blocks table keeps the fees as `total - reward`, which is what they are —
// the record carries the whole coinbase output as Total, and reward + fees
// recovers it exactly.
const insert = `insert into blocks (height, hash, ts, size, txs, miner, feesOK, minFee, avgFee,
    maxFee, txSizeMin, txSizeAvg, txSizeMax, reward, fees, difficulty)
    values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    on conflict(height) do update set hash = excluded.hash, ts = excluded.ts, size = excluded.size,
    txs = excluded.txs, miner = excluded.miner, feesOK = excluded.feesOK, minFee = excluded.minFee,
    avgFee = excluded.avgFee, maxFee = excluded.maxFee, txSizeMin = excluded.txSizeMin,
    txSizeAvg = excluded.txSizeAvg, txSizeMax = excluded.txSizeMax, reward = excluded.reward,
    fees = excluded.fees, difficulty = excluded.difficulty`

func args(bi *Info) []any {
    return []any{bi.Height, bi.Hash, bi.Time, bi.Size, bi.NumTx, bi.Miner, bi.FeesOK, bi.FeeMin,
        bi.FeeAvg, bi.FeeMax, bi.TxSizeMin, bi.TxSizeAvg, bi.TxSizeMax, bi.Reward, bi.Total - bi.Reward,
        bi.Difficulty}
}

// Columns is the order Scan reads them in, and the one every query that builds a
// record has to select.
const Columns = `height, hash, ts, size, txs, miner, feesOK, minFee, avgFee, maxFee,
    txSizeMin, txSizeAvg, txSizeMax, reward, fees, difficulty`

// Scan reads one row into a record, putting the coinbase output back together
// from the reward and the fees the table keeps separately.
func Scan(row interface{ Scan(...any) error }) (*Info, bool) {
    var bi Info
    var fees int64
    if row.Scan(&bi.Height, &bi.Hash, &bi.Time, &bi.Size, &bi.NumTx, &bi.Miner, &bi.FeesOK,
        &bi.FeeMin, &bi.FeeAvg, &bi.FeeMax, &bi.TxSizeMin, &bi.TxSizeAvg, &bi.TxSizeMax,
        &bi.Reward, &fees, &bi.Difficulty) != nil {
        return nil, false
    }
    bi.Total = bi.Reward + fees
    return &bi, true
}

// Load reads the cached record at a height.
func Load(height int64) (*Info, bool) {
    if db == nil { return nil, false }
    logging.Db("blocks: load %d", height)
    return Scan(db.QueryRow("select "+Columns+" from blocks where height = ?", height))
}

// ByTime is the highest block whose timestamp is at or before t. It is what
// turns an address's last-activity time into the block the address index scan
// can start from: every block the address appears in has a timestamp at most its
// last activity, so nothing of its history sits above that height.
func ByTime(t int64) (int64, bool) {
    if db == nil { return 0, false }
    var h int64
    var err = db.QueryRow("select height from blocks where ts <= ? order by height desc limit 1", t).Scan(&h)
    if err != nil { return 0, false }
    return h, true
}

// CirculatingSupply is the total mined at this height, in satoshi, summed one
// halving epoch at a time (at most 33 iterations rather than one per block).
//
// It is the issuance schedule, not a UTXO-set total: coins burned or lost, and
// the handful of blocks whose miners under-claimed their subsidy, are all still
// counted. `gettxoutsetinfo` would be exact but walks the whole UTXO set, which
// takes minutes — far too slow for a card that refreshes every ten.
func CirculatingSupply(height int64) int64 {
    var total int64
    for epoch := int64(0); epoch < 64; epoch++ {
        var start = epoch * 210000
        if start > height { break }
        var end = start + 210000 - 1
        if end > height { end = height }
        total += (end - start + 1) * Subsidy(start)
    }
    return total
}

// Subsidy returns the block reward in satoshi for a height from the halving
// schedule — 50 BTC, halving every 210000 blocks.
func Subsidy(height int64) int64 {
    var halvings = height / 210000
    if halvings >= 64 { return 0 }
    return int64(5000000000) >> uint(halvings)
}

// toSat converts Core's BTC to whole satoshi, as main's toSat does.
func toSat(btc float64) int64 { return int64(math.Round(btc * 1e8)) }

// feeStats summarises a block's fee distribution. Core reports each
// transaction's fee directly in getblock verbosity 2, so unlike the btcd path
// this needs no prevout fetching at all — the bounded 16-way fan-out that used
// to be here existed only because btcd made callers compute fees themselves.
// The coinbase has no fee and is skipped.
func feeStats(txs []core.Transaction) (low, avg, high int64, count int) {
    var total int64
    for i, t := range txs {
        if i == 0 { continue }
        var fee = toSat(t.Fee)
        if count == 0 || fee < low { low = fee }
        if fee > high { high = fee }
        total += fee
        count++
    }
    if count == 0 { return 0, 0, 0, 0 }
    return low, (total + int64(count)/2) / int64(count), high, count
}

// Compute builds the cached record from core: general fields from getblock
// (verbosity 2), per-tx fee stats from the fees Core reports, the
// transaction-size distribution, the block reward from the halving schedule, and
// the miner attributed from the coinbase. Reward and total (reward + fees =
// coinbase output) are always available.
func Compute(ctx context.Context, hash string) (*Info, error) {
    var blk, err = core.GetBlockVerbose(ctx, hash)
    if err != nil { return nil, err }
    if len(blk.Tx) == 0 { return nil, fmt.Errorf("block %s has no transactions", hash) }
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
    return &Info{
        Height: blk.Height, Hash: blk.Hash, Time: blk.Time, Size: blk.Size,
        NumTx: len(blk.Tx), Miner: miner,
        FeesOK: true, FeeMin: low, FeeAvg: avg, FeeMax: high,
        TxSizeMin: szMin, TxSizeAvg: int32(szSum / int64(len(blk.Tx))), TxSizeMax: szMax,
        Reward: Subsidy(blk.Height), Total: coinbaseOut, Difficulty: blk.Difficulty,
    }, nil
}

// Rollback removes the block with this hash and every block above it — what a
// block disconnected by a reorg leaves stale. Everything above goes too, since
// it was built on the disconnected block, and the highest height stored is
// where Update resumes, so the replacement chain is cached by the next
// catch-up. A hash the table does not hold removes nothing. It reports how
// many blocks it removed.
func Rollback(hash string) (int64, error) {
    if db == nil { return 0, nil }
    var res, err = db.Exec("delete from blocks where height >= (select height from blocks where hash = ?)", hash)
    if err != nil { return 0, err }
    var n, _ = res.RowsAffected()
    return n, nil
}

// Update catches the block cache up from the last processed block to tip,
// storing each block's stats in the blocks table. It is the first step of the
// one index catch-up goroutine (see startIndexUpdates), which hands it the tip
// it already fetched — it never asks the node for it itself.
//
// The place is the highest block stored: Flush commits a chunk in one
// transaction, so it advances with the batch that reached it. An empty table
// is NULL, which starts from genesis.
//
// Announced only once the records are stored: the Mini App's Blocks tab
// reads this table, so telling it earlier would have it re-fetch a list
// the new blocks are not in yet.
//
// A pause between chunks, so catching up the whole chain does not hold
// the node at full tilt for hours — but only when there is another chunk
// to come. The condition used to be `from < tip`, which is true of the
// chunk that *reaches* the tip as well, so every catch-up slept a minute
// after its last flush: a minute between a block arriving and being
// cached, now that this is the only path that caches one.
func Update(tip int64) {
    if db == nil { return }
    var last sql.NullInt64
    if err := db.QueryRow("select max(height) from blocks").Scan(&last); err != nil {
        logging.Err("blocks: last height: %v", err)
        return
    }
    var from int64
    if last.Valid { from = last.Int64 + 1 }
    var first = from
    var prog = progress.New("blocks", from, tip)
    for from <= tip {
        var to = from + chunkSize - 1
        if to > tip { to = tip }
        var infos []*Info
        for h := from; h <= to; h++ {
            var bctx, bcancel = context.WithTimeout(context.Background(), 60*time.Second)
            var hash, herr = core.GetBlockHash(bctx, h)
            bcancel()
            if herr != nil {
                logging.Warn("blocks: block %d hash: %v — retrying next run", h, herr)
                return
            }
            bctx, bcancel = context.WithTimeout(context.Background(), 60*time.Second)
            var bi, cerr = Compute(bctx, hash)
            bcancel()
            if cerr != nil {
                logging.Warn("blocks: block %d: %v — retrying next run", h, cerr)
                return
            }
            infos = append(infos, bi)
        }
        if err := Flush(infos); err != nil {
            logging.Err("blocks: flush %v", err)
            return
        }
        if len(infos) > 0 { app.Notify("blocks") }
        prog.Chunk(from, to)
        if to < tip { time.Sleep(chunkPause) }
        from = to + 1
    }
    if first <= tip {
        var bm = fmt.Sprintf("blocks %d..%d", first, from - 1)
        if first == from - 1 { bm = fmt.Sprintf("block %d", first) }
        logging.Info("blocks: collected from " + bm)
    }
}

// Flush stores a chunk of block info in one transaction. On error nothing is
// stored, so the highest height does not move and the next run retries the
// whole chunk. It is the only thing that writes the blocks table: a lookup that
// computes a block the collector has not reached shows it without storing it,
// since the highest height stored is where the collector resumes.
func Flush(bis []*Info) error {
    if db == nil { return nil }
    var tx, err = db.Begin()
    if err != nil { return err }
    defer tx.Rollback()
    var stmt, perr = tx.Prepare(insert)
    if perr != nil { return perr }
    for _, bi := range bis {
        if _, err := stmt.Exec(args(bi)...); err != nil {
            stmt.Close()
            return err
        }
    }
    if err := stmt.Close(); err != nil { return err }
    return tx.Commit()
}
