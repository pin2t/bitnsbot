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

import "database/sql"
import "bitnsbot/logging"

var db *sql.DB

// sourceURL is mempool's mining-pool definitions (pool name + coinbase output
// addresses). A package var so tests can point it at a local server.
var sourceURL = "https://raw.githubusercontent.com/mempool/mining-pools/master/pools-v2.json"
var httpClient = &http.Client{Timeout: 15 * time.Second}

// updateInterval is how often the bucket is refreshed from the source. A package
// var so tests can shrink it.
var updateInterval = 24 * time.Hour

// poolDef is a pool as mempool's definitions file describes it.
type poolDef struct {
    Name      string   `json:"name"`
    Addresses []string `json:"addresses"`
    Tags      []string `json:"tags"`
}

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

// Init stores the shared handle and builds the in-memory mappings from the rows.
func Init(handle *sql.DB) error {
    db = handle
    return loadIndex()
}

// loadIndex rebuilds the in-memory mappings from the rows. Init and update are
// the only things that change what they are built from.
//
// A pool's addresses and tags are zipped into rows positionally and padded with
// "" (see the miners table in tools/tosqlite), so an empty one is padding rather
// than an address called nothing.
func loadIndex() error {
    if db == nil { return nil }
    var addrs = map[string]string{}
    var tags []tagged
    var rows, err = db.Query("select name, address, tag from miners")
    if err != nil { return err }
    for rows.Next() {
        var name, address, tag string
        if err := rows.Scan(&name, &address, &tag); err != nil {
            rows.Close()
            return err
        }
        if address != "" { addrs[address] = name }
        if tag != "" { tags = append(tags, tagged{[]byte(tag), name}) }
    }
    rows.Close()
    if err := rows.Err(); err != nil { return err }
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

// store merges the fetched definitions into the table and reports how many
// addresses and tags were new.
//
// A pool's addresses and tags are **zipped into rows positionally** — the shape
// tools/tosqlite defines and this now writes — so adding one address changes which
// rows a pool has, not one column of one row. Each pool is therefore read, merged
// and rewritten: its rows are deleted and the zip written again, carrying the
// aggregate every row of a pool repeats. That is 171 pools of a few rows each, in
// one transaction.
func store(pools []poolDef) (added, tags int, err error) {
    if db == nil { return 0, 0, nil }
    var tx, terr = db.Begin()
    if terr != nil { return 0, 0, terr }
    defer tx.Rollback()
    for _, d := range pools {
        if d.Name == "" { continue }
        var have, aggregate, rerr = poolRows(tx, d.Name)
        if rerr != nil { return 0, 0, rerr }
        var addrs, tgs = merge(have.Addresses, d.Addresses), merge(have.Tags, d.Tags)
        if len(addrs) == len(have.Addresses) && len(tgs) == len(have.Tags) { continue }
        added += len(addrs) - len(have.Addresses)
        tags += len(tgs) - len(have.Tags)
        if _, err := tx.Exec("delete from miners where name = ?", d.Name); err != nil { return 0, 0, err }
        if err := writeZip(tx, d.Name, addrs, tgs, aggregate); err != nil { return 0, 0, err }
    }
    if err := tx.Commit(); err != nil { return 0, 0, err }
    return added, tags, loadIndex()
}

// poolRows reads what a pool's rows say: the addresses and tags they carry, and
// the aggregate they all repeat.
func poolRows(tx *sql.Tx, name string) (record, record, error) {
    var lists, aggregate record
    var rows, err = tx.Query("select address, tag, blocks, reward, fees, totalWork, lastWork from miners where name = ?", name)
    if err != nil { return lists, aggregate, err }
    defer rows.Close()
    for rows.Next() {
        var address, tag string
        var r record
        if err := rows.Scan(&address, &tag, &r.Blocks, &r.Reward, &r.Fees, &r.Work, &r.LastWork); err != nil {
            return lists, aggregate, err
        }
        if address != "" { lists.Addresses = append(lists.Addresses, address) }
        if tag != "" { lists.Tags = append(lists.Tags, tag) }
        aggregate = r
    }
    return lists, aggregate, rows.Err()
}

// writeZip pairs a pool's addresses and tags by position, padding the shorter
// with "", and writes one row each carrying the pool's aggregate. Nothing links an
// individual address to an individual tag, so the pairing means nothing beyond
// keeping the table narrow — the same reason tools/tosqlite zips them.
func writeZip(tx *sql.Tx, name string, addrs, tags []string, r record) error {
    var n = len(addrs)
    if len(tags) > n { n = len(tags) }
    if n == 0 { n = 1 }
    for i := 0; i < n; i++ {
        var address, tag string
        if i < len(addrs) { address = addrs[i] }
        if i < len(tags) { tag = tags[i] }
        var _, err = tx.Exec(`insert into miners (name, address, tag, blocks, reward, fees, totalWork, lastWork)
            values (?, ?, ?, ?, ?, ?, ?, ?)`, name, address, tag, r.Blocks, r.Reward, r.Fees, r.Work, r.LastWork)
        if err != nil { return err }
    }
    return nil
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
    var pools []poolDef
    if err := json.Unmarshal(body, &pools); err != nil {
        logging.Warn("update miners: %v", err)
        return
    }
    var added, tags int
    if added, tags, err = store(pools); err != nil {
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
