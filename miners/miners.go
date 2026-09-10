// Package miners attributes a Bitcoin block to its mining pool, backed by one
// bbolt bucket keyed by pool name: a record carries that pool's aggregated
// statistics together with the coinbase addresses and coinbase tags it is
// recognised by. The address→pool and tag→pool mappings are built from that
// bucket once at Init and kept in memory, since attribution runs for every block
// on the chain; the bucket is what survives a restart, so the definitions are not
// re-downloaded on every start. A background goroutine keeps them fresh from
// mempool's pool definitions.
package miners

import "bytes"
import "encoding/hex"
import "encoding/json"
import "io"
import "net/http"
import "sort"
import "sync"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/cursors"
import "bitnsbot/logging"

var db *bbolt.DB
var bucket = []byte("miners")

// sourceURL is mempool's mining-pool definitions (pool name + coinbase output
// addresses). A package var so tests can point it at a local server.
var sourceURL = "https://raw.githubusercontent.com/mempool/mining-pools/master/pools-v2.json"
var httpClient = &http.Client{Timeout: 15 * time.Second}

// updateInterval is how often the bucket is refreshed from the source. A package
// var so tests can shrink it.
var updateInterval = 24 * time.Hour

// tagged is one coinbase tag and the pool that writes it. The tags are a slice
// sorted by tag rather than a map, because attribution takes the first tag the
// script contains and a map's iteration order would make that a coin toss
// between two tags that both match — where the bucket cursor this replaces
// scanned in key order.
type tagged struct {
    tag  []byte
    name string
}

// The two mappings are read for every block and written only when the
// definitions are refreshed, so they are built from the bucket and held here
// rather than looked up per block.
var indexMu sync.RWMutex
var addrIndex = map[string]string{}
var tagIndex []tagged

// Init stores the shared bbolt handle, ensures the miners bucket exists, and
// builds the in-memory mappings from what is in it. The collector's place lives
// in the shared cursors bucket, which this ensures too.
func Init(handle *bbolt.DB) error {
    db = handle
    if err := cursors.Init(handle); err != nil { return err }
    var err = db.Update(func(tx *bbolt.Tx) error {
        var _, berr = tx.CreateBucketIfNotExists(bucket)
        return berr
    })
    if err != nil { return err }
    return loadIndex()
}

// loadIndex rebuilds the in-memory mappings from the bucket. Init and update are
// the only things that change what they are built from.
func loadIndex() error {
    if db == nil { return nil }
    var addrs = map[string]string{}
    var tags []tagged
    var err = db.View(func(tx *bbolt.Tx) error {
        return tx.Bucket(bucket).ForEach(func(k, v []byte) error {
            var r record
            if json.Unmarshal(v, &r) != nil { return nil }
            for _, a := range r.Addresses { addrs[a] = string(k) }
            for _, t := range r.Tags { tags = append(tags, tagged{[]byte(t), string(k)}) }
            return nil
        })
    })
    if err != nil { return err }
    sort.Slice(tags, func(i, j int) bool { return bytes.Compare(tags[i].tag, tags[j].tag) < 0 })
    indexMu.Lock()
    addrIndex, tagIndex = addrs, tags
    indexMu.Unlock()
    return nil
}

// Name returns the mining pool that owns a coinbase output address, or "" when
// the address is not a known pool address.
func Name(address string) string {
    indexMu.RLock()
    defer indexMu.RUnlock()
    return addrIndex[address]
}

// Attribute returns the mining pool that produced a block: from a coinbase output
// address when one of them is a known pool address, else from the pool tag
// embedded in the coinbase script (coinbaseHex, the scriptSig of the coinbase
// input). Returns "" when neither matches. The tag fallback carries most of the
// attribution in practice — the big pools rotate payout addresses far faster than
// the definitions list them, so on mainnet an address-only lookup misses AntPool,
// Foundry, F2Pool and friends entirely.
func Attribute(addresses []string, coinbaseHex string) string {
    indexMu.RLock()
    defer indexMu.RUnlock()
    for _, a := range addresses {
        if n := addrIndex[a]; n != "" { return n }
    }
    var script, err = hex.DecodeString(coinbaseHex)
    if err != nil || len(script) == 0 { return "" }
    for _, t := range tagIndex {
        if bytes.Contains(script, t.tag) { return t.name }
    }
    return ""
}

// empty reports whether no pool definitions are loaded yet (a fresh install that
// has never fetched the source).
func empty() bool {
    indexMu.RLock()
    defer indexMu.RUnlock()
    return len(addrIndex) == 0
}

// merge returns the union of two lists, sorted, without duplicates. What a
// record already carries is kept: the source only ever adds, so a pool that
// stops listing an address it once used still attributes the blocks it mined
// with it.
func merge(have, add []string) []string {
    var seen = map[string]bool{}
    var out []string
    for _, list := range [][]string{have, add} {
        for _, s := range list {
            if s == "" || seen[s] { continue }
            seen[s] = true
            out = append(out, s)
        }
    }
    sort.Strings(out)
    return out
}

// update fetches the pool definitions and merges each pool's addresses and tags
// into its record, leaving the aggregated statistics in it alone.
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
    type poolDef struct {
        Name      string   `json:"name"`
        Addresses []string `json:"addresses"`
        Tags      []string `json:"tags"`
    }
    var pools []poolDef
    if err := json.Unmarshal(body, &pools); err != nil {
        logging.Warn("update miners: %v", err)
        return
    }
    var added, tags int
    err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        for _, d := range pools {
            if d.Name == "" { continue }
            var r record
            if v := b.Get([]byte(d.Name)); v != nil { json.Unmarshal(v, &r) }
            var addrs, tgs = merge(r.Addresses, d.Addresses), merge(r.Tags, d.Tags)
            added += len(addrs) - len(r.Addresses)
            tags += len(tgs) - len(r.Tags)
            r.Addresses, r.Tags = addrs, tgs
            var data, merr = json.Marshal(r)
            if merr != nil { return merr }
            if err := b.Put([]byte(d.Name), data); err != nil { return err }
        }
        return nil
    })
    if err != nil {
        logging.Err("store miners: %v", err)
        return
    }
    if err := loadIndex(); err != nil {
        logging.Err("index miners: %v", err)
        return
    }
    logging.Info("miners updated: %d pools, %d new addresses, %d new tags", len(pools), added, tags)
}

// Start keeps the definitions fresh in the background: an initial fetch only when
// nothing is loaded (so a fresh install gets data, but a populated bucket is not
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
