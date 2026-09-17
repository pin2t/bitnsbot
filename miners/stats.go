package miners

import "context"
import "fmt"
import "math"
import "sort"
import "time"
import "bitnsbot/core"
import "bitnsbot/logging"
import "bitnsbot/cursors"
import "bitnsbot/signals"

// statInterval is how often the collector processes new blocks. A package var so
// tests can shrink it.
var statInterval = 10 * time.Minute
var cooldownPeriod = 1 * time.Minute

// chunkSize bounds how many blocks are aggregated in memory before a database
// flush, so catching up a large gap doesn't build one giant transaction.
var chunkSize int64 = 1000

// work per block ≈ difficulty × 2^32 (the expected number of hashes).
const workPerDifficulty = 4294967296.0
const secondsPerBlock = 600.0

// joulesPerHash assumes the most modern mining hardware: the latest hydro-cooled
// ASICs (Antminer S23-class) run at about 10 J/TH, i.e. 1e-11 J/hash. Real fleets
// mix in older, less efficient machines, so this is a lower bound on the true
// draw — the "if they ran today's best gear" figure.
const joulesPerHash = 1.0e-11

// block is the per-block data the collector needs.
type block struct {
    addresses  []string // every coinbase output address — the first known one attributes the block
    script     string   // the coinbase input's scriptSig (hex), carrying the pool tag
    reward     int64    // block subsidy + fees (satoshi) — the total coinbase output
    fees       int64    // fees only (satoshi)
    difficulty float64
}

// blockAt reads one block from the node: the header (getblock verbosity 1 →
// difficulty + txids) and the coinbase transaction, from which it reads every
// payout address, the coinbase script (which carries the pool tag) and the total
// output (subsidy + fees); fees are that total minus the height's subsidy — 50
// BTC, halving every 210000 blocks. All the coinbase addresses are kept (not just
// the first) because the pool's payout isn't always output 0.
//
// Core reports an amount as a BTC number, so the satoshi are rounded rather than
// truncated, the way the bot's toSat does.
func blockAt(ctx context.Context, height int64) (block, error) {
    var hash, err = core.GetBlockHash(ctx, height)
    if err != nil { return block{}, err }
    var header, herr = core.GetBlockTxids(ctx, hash)
    if herr != nil { return block{}, herr }
    if len(header.Tx) == 0 { return block{}, fmt.Errorf("block %d has no transactions", height) }
    var cb, cerr = core.GetRawTransaction(ctx, header.Tx[0])
    if cerr != nil { return block{}, cerr }
    var b = block{difficulty: header.Difficulty}
    for _, v := range cb.Vout {
        b.reward += int64(math.Round(v.Value * 1e8))
        if v.ScriptPubKey.Address != "" { b.addresses = append(b.addresses, v.ScriptPubKey.Address) }
    }
    if len(cb.Vin) > 0 { b.script = cb.Vin[0].Coinbase }
    b.fees = b.reward - int64(5000000000)>>uint(height/210000)
    return b, nil
}

// record is what the miners bucket holds under a pool's name: what that pool has
// mined, and the coinbase addresses and tags it is recognised by. The two lists
// are the pool definitions — they are written by update and by the migration,
// never by the collector, which reads a record only to add to its aggregates.
type record struct {
    Blocks    int64    `json:"blocks"`
    Reward    int64    `json:"reward"`   // satoshi (subsidy + fees)
    Fees      int64    `json:"fees"`     // satoshi
    Work      float64  `json:"work"`     // Σ per-block work (difficulty × 2^32 hashes)
    LastWork  float64  `json:"lastWork"` // work of this miner's most recent block
    Addresses []string `json:"addresses"`
    Tags      []string `json:"tags"`
}

// StartStats runs the by-miner statistics collector: it catches up from the last
// processed block to the current tip, then again on every block notification and
// every statInterval, whichever comes first.
func StartStats() {
    go func() {
        var wake = signals.Subscribe(signals.Block)
        collect()
        var t = time.NewTicker(statInterval)
        defer t.Stop()
        for {
            select {
            case <-t.C:
            case <-wake:
            }
            collect()
        }
    }()
}

// address list not loaded yet — nothing could be attributed
func collect() {
    if empty() { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()
    var tip, err = core.GetBlockCount(ctx)
    if err != nil {
        logging.Warn("miners stats: tip: %v", err)
        return
    }
    var last, ok = cursor()
    var from int64
    if !ok {
        from = 1
        last = 0
    } else {
        from = last + 1
    }
    var began = last
    for from <= tip {
        var to = from + chunkSize - 1
        if to > tip { to = tip }
        var deltas = map[string]*record{}
        for h := from; h <= to; h++ {
            var b, berr = blockAt(ctx, h)
            if berr != nil {
                logging.Warn("miners stats: error on block %d: %v — retry on next run", h, berr)
                return
            }
            var name = Attribute(b.addresses, b.script)
            if name == "" { continue }
            var w = b.difficulty * workPerDifficulty
            var d = deltas[name]
            if d == nil {
                d = &record{}
                deltas[name] = d
            }
            d.Blocks++
            d.Reward += b.reward
            d.Fees += b.fees
            d.Work += w
            d.LastWork = w
        }
        if err := flush(deltas, to); err != nil {
            logging.Err("miners stats: flush: %v", err)
            return
        }
        last = to
        from = to + 1
        if from < tip { time.Sleep(cooldownPeriod) }
    }
    if last > began {
        var bm = fmt.Sprintf("blocks %d..%d", from, last)
        if last == from { bm = fmt.Sprintf("block %d", from) }
        logging.Info("miners stats: collected from " + bm)
    }
}

// flush merges a chunk's in-memory deltas into the pool rows and advances the
// cursor, in one transaction. Blocks/Reward/Fees/Work accumulate; LastWork is
// overwritten with the most recent (chunks run oldest-first, so the last write
// wins). A pool's addresses and tags are rows of their own tables, so nothing
// here can disturb them.
//
// An upsert rather than an update: the pool row is there already — a block is
// only attributed to a pool mineraddr or minertag names, and those reference
// this table — but a delta is not a thing to drop on the floor if it is not.
func flush(deltas map[string]*record, last int64) error {
    if db == nil { return nil }
    var tx, err = db.Begin()
    if err != nil { return err }
    defer tx.Rollback()
    var stmt, perr = tx.Prepare(`insert into miners (name, blocks, reward, fees, totalWork, lastWork)
        values (?, ?, ?, ?, ?, ?)
        on conflict(name) do update set blocks = miners.blocks + excluded.blocks,
        reward = miners.reward + excluded.reward, fees = miners.fees + excluded.fees,
        totalWork = miners.totalWork + excluded.totalWork, lastWork = excluded.lastWork`)
    if perr != nil { return perr }
    for name, d := range deltas {
        if _, err := stmt.Exec(name, d.Blocks, d.Reward, d.Fees, d.Work, d.LastWork); err != nil {
            stmt.Close()
            return err
        }
    }
    if err := stmt.Close(); err != nil { return err }
    if err := cursors.Set(tx, cursors.Miners, last); err != nil { return err }
    return tx.Commit()
}

func cursor() (last int64, ok bool) { return cursors.Get(cursors.Miners) }

// Stat is the public per-miner view returned by Top.
type Stat struct {
    Name          string
    Blocks        int64
    Reward        int64   // satoshi (subsidy + fees)
    Fees          int64   // satoshi
    ConsumptionGW float64 // estimated power draw, gigawatts
    lastWork      float64 // work of this miner's most recent block
}

// Top returns the n miners with the most blocks mined, sorted descending. The
// consumption estimate is the miner's current hashrate — its share of the total
// blocks mined across all tracked miners times the current network hashrate
// (LastWork, the work of its most recent block, over the 10-minute block target)
// — at joulesPerHash. LastWork is what makes it *current*: it carries the
// difficulty in force now, where the accumulated Work would average in every
// past difficulty epoch.
func Top(n int) []Stat {
    var out = all()
    if len(out) > n { out = out[:n] }
    return out
}

// Get returns one miner's statistics by name. It builds the whole set because
// the consumption estimate is a *share* — a miner's blocks against every tracked
// block — so one miner's figure cannot be computed from its own record alone.
func Get(name string) (Stat, bool) {
    for _, s := range all() {
        if s.Name == name { return s, true }
    }
    return Stat{}, false
}

// Consumption is the power, in gigawatts, drawn by a pool finding share of the
// blocks at difficulty: that share of the hashrate the difficulty implies
// (difficulty × 2^32 hashes over the 600-second target), at joulesPerHash. Top's
// figures and the Mini App's miner chart both go through it, so the two cannot
// disagree about what a share of the network costs.
func Consumption(share, difficulty float64) float64 {
    return share * difficulty * workPerDifficulty / secondsPerBlock * joulesPerHash / 1e9
}

// A pool with no blocks is one the definitions name and the collector has
// never attributed a block to. It is not a statistic: reporting it would fill
// /miners with zeroes on a fresh install, and hand the app's miner page
// zeroes to present as fact.
func all() []Stat {
    if db == nil { return nil }
    var out []Stat
    var totalBlocks int64
    var rows, err = db.Query("select name, blocks, reward, fees, lastWork from miners where blocks > 0")
    if err != nil {
        logging.Err("miners: %v", err)
        return nil
    }
    defer rows.Close()
    for rows.Next() {
        var s Stat
        if err := rows.Scan(&s.Name, &s.Blocks, &s.Reward, &s.Fees, &s.lastWork); err != nil {
            logging.Err("miners: %v", err)
            return nil
        }
        totalBlocks += s.Blocks
        out = append(out, s)
    }
    var windowBlocks = float64(totalBlocks)
    for i := range out {
        if windowBlocks > 0 {
            out[i].ConsumptionGW = Consumption(float64(out[i].Blocks)/windowBlocks, out[i].lastWork/workPerDifficulty)
        }
    }
    sort.Slice(out, func(i, j int) bool {
        if out[i].Blocks != out[j].Blocks { return out[i].Blocks > out[j].Blocks }
        return out[i].Name < out[j].Name
    })
    return out
}
