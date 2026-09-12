package main

import "encoding/binary"
import "sort"
import "strconv"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/addrstat"
import "bitnsbot/app"
import "bitnsbot/logging"

// The three ranked address lists the Mini App's Addresses tab shows: how busy an
// address is, what it holds now, and how long its coins have sat still. All
// three are three readings of one record — the statistics the addrstat package
// gathers — so they are built from that bucket and nothing else writes them.
//
// Each is read through an index rather than by ranking the records on every
// request: addrstat is keyed by address, so a ranking meant scanning and sorting
// every row, where the index holds the same rows keyed by the figure they rank
// by, which a cursor walks in rank order.
type addrList struct {
    kind  string
    index []byte
    // width is how many bytes of the index key hold the value. Counts and
    // balances take eight; a last-moved date is a unix time, which fits in four
    // until 2106.
    width int
    // desc reads the index from its high end: the busiest address and the
    // largest balance rank first, where the oldest date does.
    desc bool
    // value is what this list ranks a record by, and whether the record belongs
    // in it at all — an address that has never been paid is not the poorest
    // address on the chain, and one holding nothing has no coins to have
    // abandoned. A record the scan has not reached yet is all zeroes, which is
    // why every list refuses one.
    value func(addrstat.Stat) (int64, bool)
}

// A slice rather than a map, so a build runs — and logs — in a fixed order.
var addrLists = []addrList{
    {"active", []byte("activeindex"), 8, true, func(s addrstat.Stat) (int64, bool) { return s.Txs, s.Txs > 0 }},
    {"rich", []byte("richindex"), 8, true, func(s addrstat.Stat) (int64, bool) { return s.Balance, s.Balance > 0 }},
    {"abandoned", []byte("abandonedindex"), 4, false, func(s addrstat.Stat) (int64, bool) { return s.Last, s.Balance > 0 && s.Last > 0 }},
}

// addrsFirstPage is the batch the tab opens with, addrsPage what each scroll
// adds. The first is larger because it has a screen to fill.
const addrsFirstPage = 15
const addrsPage = 10

// addrsMaxRows is how deep the lists go. Reaching row N walks N entries of the
// index, so this bounds what an edited from= can ask for — and past it a ranking
// of a shortlist has stopped saying anything anyway. Ten thousand is far beyond
// where anyone scrolls, and is the whole of an `ababuild -top 10000` list, so the
// abandoned ranking is browsable to its end.
const addrsMaxRows = 10000

// addrsRestoreRows bounds a list restored by Back, which is a separate limit
// from how deep the scroll goes: those rows arrive a batch at a time, where a
// restore renders every one of them into a single response. Twenty batches is
// the same depth the block list restores, and a reader who had scrolled past it
// loses some of it rather than being sent a megabyte of rows.
const addrsRestoreRows = addrsFirstPage + 20 * addrsPage

// addrIndexInterval is how often the indexes are rebuilt from the buckets they
// rank. A var so tests can drive the loop without waiting an hour.
var addrIndexInterval = time.Hour

// startAddrIndexes keeps the three indexes in step with their buckets, rebuilding
// every addrIndexInterval, and returns a stop that waits for a rebuild in flight
// — shutdown runs it before closeDB, since that rebuild is holding a write
// transaction.
//
// It rebuilds once immediately and then on the interval, the same shape every
// other collector here has (startBlockCache, StartStats, miners.Start). Waiting
// out the first interval instead would leave a freshly imported database showing
// three empty lists for an hour.
func startAddrIndexes() func() {
    var stop, done = make(chan struct{}), make(chan struct{})
    go func() {
        defer close(done)
        for {
            buildAddrIndexes()
            select {
            case <-time.After(addrIndexInterval):
            case <-stop:
                return
            }
        }
    }()
    return func() {
        close(stop)
        <-done
    }
}

// buildAddrIndexes rebuilds all three indexes from the addrstat records, from
// scratch: an index is dropped and written again rather than diffed, because a
// pass of the collector moves every record it touched and there is nothing
// cheaper to compare against.
//
// It reads the records **once** for all three lists rather than once per list,
// since each is a different figure out of the same record.
//
// A record the scan has not reached yet holds zero, and each list refuses one —
// an address that has never been paid is not the poorest address on the chain —
// so a rebuild during a catch-up ranks what has been gathered and leaves out
// what has not, filling in as the scan runs.
//
// The key is the whole entry: the value big-endian — so the index sorts by it
// naturally — followed by the address, and **nothing is stored as the value**.
// The address in the key is what makes it unique, and it has to be there: values
// collide heavily, and a bare value key would silently keep only the last
// address written under each one. Measured on the real exports, 120 119 active
// addresses hold just 14 269 distinct counts, so 88% of that list would have
// been lost; rich loses 65% and abandoned 34%. The value still leads the key, so
// cursor order is still rank order, and the address suffix breaks ties the same
// way the sort it replaces did. This is the shape addrindex already uses for its
// own touches, and electrs' bindex-rs before it: everything in the key, an empty
// value.
//
// The drop and the refill are **one transaction**, so a reader is served either
// the whole old index or the whole new one — never the empty middle of a
// rebuild. A list with nothing to rank ends with no index rather than a stale
// one, which is what makes a list whose records all dropped out show as empty.
func buildAddrIndexes() {
    if db == nil { return }
    var byList = make([][][]byte, len(addrLists))
    var err = addrstat.ForEach(func(addr string, s addrstat.Stat) {
        for i, l := range addrLists {
            var n, ok = l.value(s)
            if !ok || n < 0 { continue }
            // a value too wide for the key would truncate and rank wrongly
            if l.width == 4 && n > 0xffffffff { continue }
            var key = make([]byte, l.width + len(addr))
            if l.width == 8 {
                binary.BigEndian.PutUint64(key, uint64(n))
            } else {
                binary.BigEndian.PutUint32(key, uint32(n))
            }
            copy(key[l.width:], addr)
            byList[i] = append(byList[i], key)
        }
    })
    if err != nil {
        logging.Err("read address statistics: %v", err)
        return
    }
    for i, l := range addrLists {
        var started = time.Now()
        var keys = byList[i]
        // Sorted before they are written, which is the whole cost of this: rows
        // come out of the source in address order, which is random against the
        // index key, and bbolt rebalances on every such insert. Measured on the
        // real exports, sorting first took the three builds from 3.45 s to
        // 106 ms.
        sort.Slice(keys, func(i, j int) bool { return string(keys[i]) < string(keys[j]) })
        var err = db.Update(func(tx *bbolt.Tx) error {
            if tx.Bucket(l.index) != nil {
                if derr := tx.DeleteBucket(l.index); derr != nil { return derr }
            }
            if len(keys) == 0 { return nil }
            var b, berr = tx.CreateBucket(l.index)
            if berr != nil { return berr }
            for _, key := range keys {
                if perr := b.Put(key, nil); perr != nil { return perr }
            }
            return nil
        })
        if err != nil {
            logging.Err("build %s: %v", l.index, err)
            continue
        }
        logging.Info("rebuilt %s: %d addresses in %s", l.index, len(keys), time.Since(started).Round(time.Millisecond))
    }
}

// Addresses reads one window of one ranked list, by walking that list's index
// from the end the ranking starts at. There is no scan and no sort: a batch
// costs the steps it skips plus the rows it returns.
func (appSource) Addresses(lang string, rng app.AddrRange) app.Addrs {
    var out = app.Addrs{Kind: rng.Kind}
    var list addrList
    for _, l := range addrLists {
        if l.kind == rng.Kind { list = l }
    }
    if db == nil || list.kind == "" || rng.From >= addrsMaxRows { return out }
    var want = addrsPage
    if rng.From == 0 { want = addrsFirstPage }
    // A restored list is as deep as the reader had scrolled, and always at least
    // the batch the tab opens with — restoring row 0 alone would be a one-row
    // list that the sentinel then had to refill.
    if rng.Restore && rng.Down + 1 > want {
        want = rng.Down + 1
        if want > addrsRestoreRows { want = addrsRestoreRows }
    }
    if rng.From + want > addrsMaxRows { want = addrsMaxRows - rng.From }
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(list.index)
        if b == nil { return nil }
        var c = b.Cursor()
        var k, _ = c.First()
        var step = func() []byte { var n, _ = c.Next(); return n }
        if list.desc {
            k, _ = c.Last()
            step = func() []byte { var p, _ = c.Prev(); return p }
        }
        for i := 0; i < rng.From && k != nil; i++ { k = step() }
        for ; k != nil && len(out.Rows) < want; k = step() {
            if len(k) <= list.width { continue }
            var n int64
            if list.width == 8 {
                n = int64(binary.BigEndian.Uint64(k[:8]))
            } else {
                n = int64(binary.BigEndian.Uint32(k[:4]))
            }
            var value string
            switch list.kind {
            case "active":
                value = i18nl(lang).Sprintf("%s txs", group(n))
            case "rich":
                // The same shape btcAmount gives, without its USD tail: a list
                // row has one column for this, and the price belongs on the
                // details page. Under a whole coin the satoshi are kept, or
                // every small balance would render as "0 BTC".
                if n >= 1e8 {
                    value = strconv.FormatFloat(toBTC(n), 'f', 2, 64) + " BTC"
                } else {
                    value = trimZeros(strconv.FormatFloat(toBTC(n), 'f', 8, 64)) + " BTC"
                }
            case "abandoned":
                value = day(n, lang)
            }
            var addr = string(k[list.width:])
            out.Rows = append(out.Rows, app.Addr{Short: short(addr), Id: addr,
                Value: value, Idx: rng.From + len(out.Rows)})
        }
        // the post statement has already stepped past the last row taken, so
        // this is the entry the next batch would start at
        out.More = k != nil
        return nil
    })
    out.Next = rng.From + len(out.Rows)
    if out.Next >= addrsMaxRows { out.More = false }
    out.OK = len(out.Rows) > 0
    return out
}
