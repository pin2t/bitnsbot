// Package miners attributes a Bitcoin block to its mining pool from a coinbase
// output address, backed by a bbolt bucket (address → pool name) rather than an
// in-memory map rebuilt from GitHub on every start. A background goroutine keeps
// the bucket fresh from mempool's pool definitions.
package miners

import "bytes"
import "encoding/hex"
import "encoding/json"
import "io"
import "net/http"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

var db *bbolt.DB
var bucket = []byte("miners")
var tagBucket = []byte("miners-tag")

// sourceURL is mempool's mining-pool definitions (pool name + coinbase output
// addresses). A package var so tests can point it at a local server.
var sourceURL = "https://raw.githubusercontent.com/mempool/mining-pools/master/pools-v2.json"

var httpClient = &http.Client{Timeout: 15 * time.Second}

// updateInterval is how often the bucket is refreshed from the source. A package
// var so tests can shrink it.
var updateInterval = 24 * time.Hour

type poolDef struct {
    Name      string   `json:"name"`
    Addresses []string `json:"addresses"`
    Tags      []string `json:"tags"`
}

// Init stores the shared bbolt handle and ensures the buckets exist: `miners`
// (address → pool name), `miners-tag` (coinbase tag → pool name), `miners-stat`
// (pool name → aggregated stats), and `miners-cursor` (the collector's cursor).
func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        for _, name := range [][]byte{bucket, tagBucket, statBucket, cursorBucket} {
            if _, err := tx.CreateBucketIfNotExists(name); err != nil { return err }
        }
        return nil
    })
}

// Name returns the mining pool that owns a coinbase output address, or "" when
// the address is not a known pool address. Reads only from the database.
func Name(address string) string {
    if db == nil { return "" }
    var name string
    db.View(func(tx *bbolt.Tx) error {
        if v := tx.Bucket(bucket).Get([]byte(address)); v != nil { name = string(v) }
        return nil
    })
    return name
}

// Attribute returns the mining pool that produced a block: from a coinbase output
// address when one of them is a known pool address (an exact key lookup), else
// from the pool tag embedded in the coinbase script (coinbaseHex, the scriptSig of
// the coinbase input). Returns "" when neither matches. The tag fallback carries
// most of the attribution in practice — the big pools rotate payout addresses far
// faster than the definitions list them, so on mainnet an address-only lookup
// misses AntPool, Foundry, F2Pool and friends entirely. Tags are matched from the
// database (a couple hundred rows scanned per block), not an in-memory copy.
func Attribute(addresses []string, coinbaseHex string) string {
    for _, a := range addresses {
        if n := Name(a); n != "" { return n }
    }
    if db == nil { return "" }
    var script, err = hex.DecodeString(coinbaseHex)
    if err != nil || len(script) == 0 { return "" }
    var name string
    db.View(func(tx *bbolt.Tx) error {
        var c = tx.Bucket(tagBucket).Cursor()
        for k, v := c.First(); k != nil; k, v = c.Next() {
            if bytes.Contains(script, k) { name = string(v); break }
        }
        return nil
    })
    return name
}

// empty reports whether the bucket holds no addresses yet (a fresh install that
// has never fetched the source).
func empty() bool {
    if db == nil { return true }
    var e = true
    db.View(func(tx *bbolt.Tx) error {
        var k, _ = tx.Bucket(bucket).Cursor().First()
        e = k == nil
        return nil
    })
    return e
}

// update fetches the pool definitions and Puts each pool address → name. It never
// deletes: the source only ever adds addresses, so old ones stay relevant and a
// full re-sync is unnecessary.
func update() {
    logging.Net("miners → GET %s", sourceURL)
    var resp, err = httpClient.Get(sourceURL)
    if err != nil {
        logging.Warn("update miners: %v", err)
        return
    }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil {
        logging.Warn("update miners: %v", readErr)
        return
    }
    if resp.StatusCode != http.StatusOK {
        logging.Warn("update miners: status %d", resp.StatusCode)
        return
    }
    var defs []poolDef
    if err := json.Unmarshal(body, &defs); err != nil {
        logging.Warn("update miners: %v", err)
        return
    }
    var added, tagged int
    err = db.Update(func(tx *bbolt.Tx) error {
        var b, tb = tx.Bucket(bucket), tx.Bucket(tagBucket)
        for _, d := range defs {
            for _, a := range d.Addresses {
                if b.Get([]byte(a)) == nil { added++ }
                if err := b.Put([]byte(a), []byte(d.Name)); err != nil { return err }
            }
            for _, t := range d.Tags {
                if tb.Get([]byte(t)) == nil { tagged++ }
                if err := tb.Put([]byte(t), []byte(d.Name)); err != nil { return err }
            }
        }
        return nil
    })
    if err != nil {
        logging.Err("store miners: %v", err)
        return
    }
    logging.Info("miners updated: %d pools, %d new addresses, %d new tags", len(defs), added, tagged)
}

// Start keeps the bucket fresh in the background: an initial fetch only when the
// bucket is empty (so a fresh install gets data, but a populated one is not
// re-downloaded on every restart), then a refresh every updateInterval.
func Start() {
    go func() {
        if empty() { update() }
        var t = time.NewTicker(updateInterval)
        defer t.Stop()
        for range t.C {
            update()
        }
    }()
}
