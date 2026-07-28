package miners

import "context"
import "encoding/json"
import "sort"
import "strconv"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

var statBucket = []byte("miners-stat")
var cursorBucket = []byte("miners-cursor")

// statInterval is how often the collector processes new blocks. A package var so
// tests can shrink it.
var statInterval = 10 * time.Minute

// chunkSize bounds how many blocks are aggregated in memory before a database
// flush, so catching up a large gap doesn't build one giant transaction.
var chunkSize int64 = 1000

// initialWindow is how far back the collector starts on a fresh install (no
// cursor yet), so /miners has data without walking the whole chain from genesis.
var initialWindow int64 = 1000

// work per block ≈ difficulty × 2^32 (the expected number of hashes).
const workPerDifficulty = 4294967296.0
const secondsPerBlock = 600.0

// joulesPerHash assumes the most modern mining hardware: the latest hydro-cooled
// ASICs (Antminer S23-class) run at about 10 J/TH, i.e. 1e-11 J/hash. Real fleets
// mix in older, less efficient machines, so this is a lower bound on the true
// draw — the "if they ran today's best gear" figure.
const joulesPerHash = 1.0e-11

// Block is the per-block data the collector needs; the Source (implemented by the
// caller, which owns the btcd connection) supplies it.
type Block struct {
    CoinbaseAddresses []string // every coinbase output address — the first known one attributes the block
    CoinbaseScript    string   // the coinbase input's scriptSig (hex), carrying the pool tag
    Reward            float64  // block subsidy + fees (BTC) — the total coinbase output
    Fees              float64  // fees only (BTC)
    Difficulty        float64
}

// Source supplies the chain data the collector reads.
type Source interface {
    Tip(ctx context.Context) (int64, error)
    Block(ctx context.Context, height int64) (Block, error)
}

// stat is the stored per-miner aggregate (keyed by miner name in miners-stat).
type stat struct {
    Blocks   int64
    Reward   float64 // BTC (subsidy + fees)
    Fees     float64 // BTC
    Work     float64 // Σ per-block work (difficulty × 2^32 hashes)
    LastWork float64 // work of this miner's most recent block
}

// StartStats runs the by-miner statistics collector: it catches up from the last
// processed block to the current tip, then again every statInterval. src supplies
// the chain data (it owns the btcd connection).
func StartStats(src Source) {
    go func() {
        collect(src)
        var t = time.NewTicker(statInterval)
        defer t.Stop()
        for range t.C {
            collect(src)
        }
    }()
}

func collect(src Source) {
    if empty() { return } // address list not loaded yet — nothing could be attributed
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()
    var tip, err = src.Tip(ctx)
    if err != nil {
        logging.Warn("miners stats: tip: %v", err)
        return
    }
    var last, ok = loadCursor()
    var from int64
    if !ok {
        from = tip - initialWindow + 1
        if from < 0 { from = 0 }
        last = from - 1
    } else {
        from = last + 1
    }
    var began = last
    for from <= tip {
        var to = from + chunkSize - 1
        if to > tip { to = tip }
        var deltas = map[string]*stat{}
        for h := from; h <= to; h++ {
            var b, berr = src.Block(ctx, h)
            if berr != nil {
                // abandon the run without flushing, so the cursor stays put and
                // the next one retries this range — advancing past a block that
                // failed to fetch (a btcd restart mid-catch-up fails every block
                // in the chunk) would drop it from the aggregate for good
                logging.Warn("miners stats: block %d: %v — retrying next run", h, berr)
                return
            }
            var name = Attribute(b.CoinbaseAddresses, b.CoinbaseScript)
            if name == "" { continue }
            var w = b.Difficulty * workPerDifficulty
            var d = deltas[name]
            if d == nil {
                d = &stat{}
                deltas[name] = d
            }
            d.Blocks++
            d.Reward += b.Reward
            d.Fees += b.Fees
            d.Work += w
            d.LastWork = w
        }
        if err := flush(deltas, to); err != nil {
            logging.Err("miners stats: flush: %v", err)
            return
        }
        last = to
        from = to + 1
    }
    if last > began {
        logging.Info("miners stats: processed %d blocks, up to %d", last-began, last)
    }
}

// flush merges a chunk's in-memory deltas into miners-stat and advances the
// cursor, in one transaction. Blocks/Reward/Fees/Work accumulate; LastWork is
// overwritten with the most recent (chunks run oldest-first, so the last write
// wins).
func flush(deltas map[string]*stat, last int64) error {
    return db.Update(func(tx *bbolt.Tx) error {
        var sb = tx.Bucket(statBucket)
        for name, d := range deltas {
            var s stat
            if v := sb.Get([]byte(name)); v != nil { json.Unmarshal(v, &s) }
            s.Blocks += d.Blocks
            s.Reward += d.Reward
            s.Fees += d.Fees
            s.Work += d.Work
            s.LastWork = d.LastWork
            var data, err = json.Marshal(s)
            if err != nil { return err }
            if err := sb.Put([]byte(name), data); err != nil { return err }
        }
        return tx.Bucket(cursorBucket).Put(
            []byte("cursor"), []byte(strconv.FormatInt(last, 10)))
    })
}

func loadCursor() (last int64, ok bool) {
    if db == nil { return 0, false }
    db.View(func(tx *bbolt.Tx) error {
        if v := tx.Bucket(cursorBucket).Get([]byte("cursor")); v != nil {
            var err error
            last, err = strconv.ParseInt(string(v), 10, 64)
            if err == nil { ok = true }
        }
        return nil
    })
    return
}

// Stat is the public per-miner view returned by Top.
type Stat struct {
    Name          string
    Blocks        int64
    Reward        float64 // BTC (subsidy + fees)
    Fees          float64 // BTC
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
    if db == nil { return nil }
    var out []Stat
    var totalBlocks int64
    db.View(func(tx *bbolt.Tx) error {
        return tx.Bucket(statBucket).ForEach(func(k, v []byte) error {
            var s stat
            if json.Unmarshal(v, &s) != nil { return nil }
            totalBlocks += s.Blocks
            out = append(out, Stat{Name: string(k), Blocks: s.Blocks, Reward: s.Reward, Fees: s.Fees, lastWork: s.LastWork})
            return nil
        })
    })
    var windowBlocks = float64(totalBlocks)
    for i := range out {
        if windowBlocks > 0 {
            out[i].ConsumptionGW = (float64(out[i].Blocks) / windowBlocks) * (out[i].lastWork / secondsPerBlock) * joulesPerHash / 1e9
        }
    }
    sort.Slice(out, func(i, j int) bool {
        if out[i].Blocks != out[j].Blocks { return out[i].Blocks > out[j].Blocks }
        return out[i].Name < out[j].Name
    })
    if len(out) > n { out = out[:n] }
    return out
}
