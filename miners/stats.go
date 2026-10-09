package miners

import "context"
import "fmt"
import "math"
import "sort"
import "time"
import "bitnsbot/core"
import "bitnsbot/logging"
import "bitnsbot/cursors"

// cooldownPeriod is how long a catch-up rests after each chunk of a long
// gap, so hours of catch-up do not hold the node at full tilt for all of it. A
// package var so tests do not sit through it.
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

// Update aggregates every block from the cursor to tip into the per-pool
// rows. The bot's one index catch-up goroutine calls it after fetching the
// tip once, so the tip parameter is what a block notification's one tip read
// serves.
func Update(tip int64) {
    if empty() { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()
    var last, ok = cursor()
    var from int64
    if !ok {
        from = 1
        last = 0
    } else {
        from = last + 1
    }
    var first = from
    for from <= tip {
        var to = from + chunkSize - 1
        if to > tip { to = tip }
        var deltas = map[string]struct{ blocks, reward, fees int64; work, lastWork float64 }{}
        for h := from; h <= to; h++ {
            var hash, err = core.GetBlockHash(ctx, h)
            var header *core.BlockTxids
            if err == nil { header, err = core.GetBlockTxids(ctx, hash) }
            if err == nil && len(header.Tx) == 0 { err = fmt.Errorf("block %d has no transactions", h) }
            var cb *core.Transaction
            if err == nil { cb, err = core.GetRawTransaction(ctx, header.Tx[0]) }
            if err != nil {
                logging.Warn("miners stats: error on block %d: %v — retry on next run", h, err)
                return
            }
            var reward int64
            var addresses []string
            for _, v := range cb.Vout {
                reward += int64(math.Round(v.Value * 1e8))
                if v.ScriptPubKey.Address != "" { addresses = append(addresses, v.ScriptPubKey.Address) }
            }
            var script string
            if len(cb.Vin) > 0 { script = cb.Vin[0].Coinbase }
            var name = Attribute(addresses, script)
            if name == "" { continue }
            var w = header.Difficulty * workPerDifficulty
            var d = deltas[name]
            d.blocks++
            d.reward += reward
            d.fees += reward - int64(5000000000)>>uint(h/210000)
            d.work += w
            d.lastWork = w
            deltas[name] = d
        }
        if db == nil { return }
        var tx, err = db.Begin()
        if err != nil {
            logging.Err("miners stats: flush: %v", err)
            return
        }
        var stmt, prepErr = tx.Prepare(`insert into miners (name, blocks, reward, fees, totalWork, lastWork)
            values (?, ?, ?, ?, ?, ?)
            on conflict(name) do update set blocks = miners.blocks + excluded.blocks,
            reward = miners.reward + excluded.reward, fees = miners.fees + excluded.fees,
            totalWork = miners.totalWork + excluded.totalWork, lastWork = excluded.lastWork`)
        if prepErr != nil {
            tx.Rollback()
            logging.Err("miners stats: flush: %v", prepErr)
            return
        }
        for name, d := range deltas {
            if _, err := stmt.Exec(name, d.blocks, d.reward, d.fees, d.work, d.lastWork); err != nil {
                stmt.Close()
                tx.Rollback()
                logging.Err("miners stats: flush: %v", err)
                return
            }
        }
        stmt.Close()
        if err := cursors.Set(tx, cursors.Miners, to); err != nil {
            tx.Rollback()
            logging.Err("miners stats: flush: %v", err)
            return
        }
        if err := tx.Commit(); err != nil {
            logging.Err("miners stats: flush: %v", err)
            return
        }
        last = to
        from = to + 1
        if from < tip { time.Sleep(cooldownPeriod) }
    }
    if last >= first {
        var bm = fmt.Sprintf("blocks %d..%d", first, last)
        if last == from { bm = fmt.Sprintf("block %d", first) }
        logging.Info("miners stats: collected from " + bm)
    }
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
