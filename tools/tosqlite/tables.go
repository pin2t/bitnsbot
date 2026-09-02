package main

import "bytes"
import "database/sql"
import "encoding/binary"
import "encoding/hex"
import "encoding/json"
import "math"
import "sort"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

// The persisted forms, mirrored from the packages that write them — package main
// and bitnsbot/{rates,miners,watches} keep them unexported, so this tool cannot
// import them. The JSON tags are the contract; they must not drift.
type blockInfo struct {
    Height     int64   `json:"height"`
    Hash       string  `json:"hash"`
    Time       int64   `json:"timestamp"`
    Size       int32   `json:"size"`
    NumTx      int     `json:"txCount"`
    Miner      string  `json:"miner"`
    FeesOK     bool    `json:"feesOk"`
    FeeMin     int64   `json:"minFee"`
    FeeAvg     int64   `json:"avgFee"`
    FeeMax     int64   `json:"maxFee"`
    TxSizeMin  int32   `json:"txSizeMin"`
    TxSizeAvg  int32   `json:"txSizeAvg"`
    TxSizeMax  int32   `json:"txSizeMax"`
    Reward     int64   `json:"reward"`
    Total      int64   `json:"total"`
    Difficulty float64 `json:"difficulty"`
}

type marketRecord struct {
    Timestamp int64   `json:"timestamp"`
    Price     float64 `json:"price"`
    MarketCap float64 `json:"marketCap"`
    Volume24h float64 `json:"volume24h"`
}

type minerStat struct {
    Blocks   int64   `json:"blocks"`
    Reward   int64   `json:"reward"`
    Fees     int64   `json:"fees"`
    Work     float64 `json:"work"`
    LastWork float64 `json:"lastWork"`
}

// a rate's timestamp lives in the bbolt key, not the value — the stored struct
// tags it `json:"-"` — so copyRates must read it from there.
type rateRecord struct {
    Cents int64 `json:"cents"`
}

type watchRecord struct {
    Created int64  `json:"created"`
    Chat    int64  `json:"chat"`
    Watch   string `json:"watch"`
    Alias   string `json:"alias"`
}

// progressInterval bounds how often a running copy reports. The address index is
// hours long, and a silent run is indistinguishable from a hung one.
var progressInterval = 30 * time.Second

// writer batches inserts into transactions of -batch rows. One transaction for a
// whole table would hold every row until commit, and one per row would fsync
// millions of times.
type writer struct {
    db       *sql.DB
    table    string
    query    string
    tx       *sql.Tx
    stmt     *sql.Stmt
    pending  int
    rows     int
    began    time.Time
    reported time.Time
}

func newWriter(db *sql.DB, table, query string) *writer {
    var now = time.Now()
    return &writer{db: db, table: table, query: query, began: now, reported: now}
}

func (w *writer) add(args ...any) error {
    if w.tx == nil {
        var tx, err = w.db.Begin()
        if err != nil { return err }
        stmt, err := tx.Prepare(w.query)
        if err != nil {
            tx.Rollback()
            return err
        }
        w.tx, w.stmt = tx, stmt
    }
    if _, err := w.stmt.Exec(args...); err != nil {
        w.tx.Rollback()
        w.stmt, w.tx, w.pending = nil, nil, 0
        return err
    }
    w.rows++
    w.pending++
    if w.pending >= *batch { return w.flush() }
    return nil
}

func (w *writer) flush() error {
    if w.tx == nil { return nil }
    var stmt, tx = w.stmt, w.tx
    w.stmt, w.tx, w.pending = nil, nil, 0
    if err := stmt.Close(); err != nil {
        tx.Rollback()
        return err
    }
    if err := tx.Commit(); err != nil { return err }
    if time.Since(w.reported) >= progressInterval {
        w.reported = time.Now()
        logging.Status("%s: %d rows in %s", w.table, w.rows, time.Since(w.began).Round(time.Second))
    }
    return nil
}

func copyBlocks(source *bbolt.DB, target *sql.DB) (rows, skipped int, err error) {
    var w = newWriter(target, "blocks", `insert into blocks (height, hash, ts, size, txs, miner,
        feesOK, minFee, avgFee, maxFee, txSizeMin, txSizeAvg, txSizeMax, reward, fees, difficulty)
        values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
    err = source.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("blocks-stat"))
        if b == nil { return nil }
        return b.ForEach(func(k, v []byte) error {
            var bi blockInfo
            if json.Unmarshal(v, &bi) != nil {
                skipped++
                return nil
            }
            logging.Db("blocks: %d", bi.Height)
            var feesOK int
            if bi.FeesOK { feesOK = 1 }
            // the record keeps reward and total, where total is the whole coinbase
            // output (reward + fees), so the fees column is their difference
            if err := w.add(bi.Height, bi.Hash, bi.Time, bi.Size, bi.NumTx, bi.Miner, feesOK,
                bi.FeeMin, bi.FeeAvg, bi.FeeMax, bi.TxSizeMin, bi.TxSizeAvg, bi.TxSizeMax,
                bi.Reward, bi.Total-bi.Reward, bi.Difficulty); err != nil { return err }
            rows++
            return nil
        })
    })
    if err != nil { return rows, skipped, err }
    return rows, skipped, w.flush()
}

func copyMarket(source *bbolt.DB, target *sql.DB) (rows, skipped int, err error) {
    var w = newWriter(target, "market", "insert into market (ts, price, cap, volume24h) values (?, ?, ?, ?)")
    err = source.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("market"))
        if b == nil { return nil }
        return b.ForEach(func(k, v []byte) error {
            var m marketRecord
            if json.Unmarshal(v, &m) != nil {
                skipped++
                return nil
            }
            logging.Db("market: %d", m.Timestamp)
            // all three are stored as floating-point USD and land as integer cents,
            // the unit the rates bucket already uses to keep prices off floats
            if err := w.add(m.Timestamp, cents(m.Price), cents(m.MarketCap), cents(m.Volume24h)); err != nil { return err }
            rows++
            return nil
        })
    })
    if err != nil { return rows, skipped, err }
    return rows, skipped, w.flush()
}

func cents(usd float64) int64 { return int64(math.Round(usd * 100)) }

// copyMiners folds three buckets into one table: miners maps a coinbase output
// address to its pool, miners-tag a coinbase tag to the same pool, and miners-stat
// holds that pool's aggregate. A pool's addresses and tags are zipped positionally
// into rows and padded with "" — the buckets record which pool each belongs to but
// no link between an individual address and an individual tag, so the pairing
// within a pool carries no meaning beyond keeping the table narrow.
func copyMiners(source *bbolt.DB, target *sql.DB) (rows, skipped int, err error) {
    var addrs = map[string][]string{}
    var tags = map[string][]string{}
    var stats = map[string]minerStat{}
    err = source.View(func(tx *bbolt.Tx) error {
        for _, b := range []struct {
            bucket string
            into   map[string][]string
        }{{"miners", addrs}, {"miners-tag", tags}} {
            var bucket = tx.Bucket([]byte(b.bucket))
            if bucket == nil { continue }
            // both buckets are keyed by the address or tag and valued by the pool
            // name, so they are inverted here to gather each pool's keys together
            var err = bucket.ForEach(func(k, v []byte) error {
                b.into[string(v)] = append(b.into[string(v)], string(k))
                return nil
            })
            if err != nil { return err }
        }
        var bucket = tx.Bucket([]byte("miners-stat"))
        if bucket == nil { return nil }
        return bucket.ForEach(func(k, v []byte) error {
            var s minerStat
            if json.Unmarshal(v, &s) != nil {
                skipped++
                return nil
            }
            stats[string(k)] = s
            return nil
        })
    })
    if err != nil { return 0, skipped, err }
    var names []string
    for name := range addrs { names = append(names, name) }
    for name := range tags {
        if _, ok := addrs[name]; !ok { names = append(names, name) }
    }
    for name := range stats {
        if _, ok := addrs[name]; ok { continue }
        if _, ok := tags[name]; ok { continue }
        names = append(names, name)
    }
    sort.Strings(names)
    var w = newWriter(target, "miners", `insert into miners (name, address, tag, blocks, reward,
        fees, totalWork, lastWork) values (?, ?, ?, ?, ?, ?, ?, ?)`)
    for _, name := range names {
        var a, t, s = addrs[name], tags[name], stats[name]
        logging.Db("miners: %s (%d addresses, %d tags)", name, len(a), len(t))
        // a pool known only from miners-stat still gets its one row of aggregates
        for i := 0; i < max(len(a), len(t), 1); i++ {
            var address, tag string
            if i < len(a) { address = a[i] }
            if i < len(t) { tag = t[i] }
            if err := w.add(name, address, tag, s.Blocks, s.Reward, s.Fees, s.Work, s.LastWork); err != nil {
                return rows, skipped, err
            }
            rows++
        }
    }
    return rows, skipped, w.flush()
}

// copyRates takes each timestamp from the bbolt key. It is stored nowhere else —
// the value holds only the price, tagged `json:"-"` in the bot — so a key that has
// been through dbui's CSV export and come back still wearing the `hex:` marker
// that UI renders binary keys behind is decoded rather than dropped, which would
// otherwise lose the whole table.
func copyRates(source *bbolt.DB, target *sql.DB) (rows, skipped int, err error) {
    var w = newWriter(target, "rates", "insert into rates (ts, cents) values (?, ?)")
    var encoded int
    err = source.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("rates"))
        if b == nil { return nil }
        return b.ForEach(func(k, v []byte) error {
            if bytes.HasPrefix(k, []byte("hex:")) {
                if decoded, derr := hex.DecodeString(string(k[len("hex:"):])); derr == nil {
                    k = decoded
                    encoded++
                }
            }
            var r rateRecord
            if len(k) != 8 || json.Unmarshal(v, &r) != nil {
                skipped++
                return nil
            }
            var ts = int64(binary.BigEndian.Uint64(k))
            logging.Db("rates: %d", ts)
            if err := w.add(ts, r.Cents); err != nil { return err }
            rows++
            return nil
        })
    })
    if err != nil { return rows, skipped, err }
    if encoded > 0 {
        logging.Warn("rates: %d keys were dbui-encoded (hex:) rather than raw — the source was rebuilt from a CSV export without decoding them", encoded)
    }
    return rows, skipped, w.flush()
}

// copyWatches collapses duplicates. The bucket stores one record per /watch under
// an auto-incrementing key and never deduplicates, so one chat can hold the same
// address twice, which the table's (chat, addr) key cannot; the last record wins,
// and how many were folded away is reported.
func copyWatches(source *bbolt.DB, target *sql.DB) (rows, skipped int, err error) {
    var w = newWriter(target, "watches", "insert or replace into watches (chat, addr, alias, created) values (?, ?, ?, ?)")
    var seen = map[string]bool{}
    var duplicates int
    err = source.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("watches"))
        if b == nil { return nil }
        return b.ForEach(func(k, v []byte) error {
            var r watchRecord
            if json.Unmarshal(v, &r) != nil {
                skipped++
                return nil
            }
            logging.Db("watches: chat=%d addr=%s", r.Chat, r.Watch)
            if err := w.add(r.Chat, r.Watch, r.Alias, r.Created); err != nil { return err }
            var key = string(itob(uint64(r.Chat))) + r.Watch
            if seen[key] {
                duplicates++
            } else {
                seen[key], rows = true, rows+1
            }
            return nil
        })
    })
    if err != nil { return rows, skipped, err }
    if duplicates > 0 {
        logging.Warn("watches: %d duplicate (chat, address) watches collapsed", duplicates)
    }
    return rows, skipped, w.flush()
}

// copyAddrindex packs the bbolt key into the single shard column. That key is a
// 2-byte shard and a 4-byte block-range index, both big-endian, and reading all
// six bytes as one integer keeps them in the same order the index relies on.
func copyAddrindex(source *bbolt.DB, target *sql.DB) (rows, skipped int, err error) {
    var w = newWriter(target, "addrindex", "insert into addrindex (shard, data) values (?, ?)")
    err = source.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("addrindex"))
        if b == nil { return nil }
        return b.ForEach(func(k, v []byte) error {
            if len(k) != 6 {
                skipped++
                return nil
            }
            var shard int64
            for _, c := range k { shard = shard<<8 | int64(c) }
            // bbolt's values are mmap'd and valid only inside this transaction
            if err := w.add(shard, append([]byte(nil), v...)); err != nil { return err }
            rows++
            return nil
        })
    })
    if err != nil { return rows, skipped, err }
    return rows, skipped, w.flush()
}

func itob(v uint64) []byte {
    var buf = make([]byte, 8)
    binary.BigEndian.PutUint64(buf, v)
    return buf
}
