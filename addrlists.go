package main

import "encoding/binary"
import "sort"
import "strconv"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/app"
import "bitnsbot/logging"

// The three ranked address lists the Mini App's Addresses tab shows: how busy an
// address is, what it holds now, and how long its coins have sat still.
// tools/csvimport fills the source buckets from the exports tools/addrindex
// produces — nothing in the bot writes them.
//
// Each is read through an index rather than the bucket itself. A bucket is keyed
// by address and ordered by address, so ranking it meant scanning and sorting
// every row on every request; the index holds the same rows keyed by the value,
// which a cursor walks in rank order.
type addrList struct {
    kind   string
    bucket []byte
    index  []byte
    // width is how many bytes of the index key hold the value. Counts and
    // balances take eight; a last-moved date is a unix time, which fits in four
    // until 2106.
    width int
    // desc reads the index from its high end: the busiest address and the
    // largest balance rank first, where the oldest date does.
    desc bool
}

// A slice rather than a map, so a build runs — and logs — in a fixed order.
var addrLists = []addrList{
    {"active", []byte("active"), []byte("activeindex"), 8, true},
    {"rich", []byte("rich"), []byte("richindex"), 8, true},
    {"abandoned", []byte("abandoned"), []byte("abandonedindex"), 4, false},
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

// buildAddrIndexes creates each list's index, once, at startup.
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
// An index that is already there is left alone, so this costs nothing after the
// first start — and so it does not notice its source changing. The buckets are
// loaded offline by tools/csvimport, so re-importing one means dropping its
// index (the database UI does that) for the next start to rebuild it. A source
// bucket that is missing or holds nothing indexable is left without an index at
// all rather than with an empty one, or a later import would never be indexed.
func buildAddrIndexes() {
    if db == nil { return }
    for _, l := range addrLists {
        var started = time.Now()
        var keys [][]byte
        var err = db.View(func(tx *bbolt.Tx) error {
            if tx.Bucket(l.index) != nil { return nil }
            var b = tx.Bucket(l.bucket)
            if b == nil { return nil }
            return b.ForEach(func(k, v []byte) error {
                // csvimport writes these values as decimal text. Anything else
                // is a row from somewhere this cannot read, and a value too wide
                // for the key would truncate and rank wrongly, so both are
                // skipped rather than indexed as something they are not.
                var n, cerr = strconv.ParseInt(string(v), 10, 64)
                if cerr != nil || n < 0 { return nil }
                if l.width == 4 && n > 0xffffffff { return nil }
                var key = make([]byte, l.width + len(k))
                if l.width == 8 {
                    binary.BigEndian.PutUint64(key, uint64(n))
                } else {
                    binary.BigEndian.PutUint32(key, uint32(n))
                }
                copy(key[l.width:], k)
                keys = append(keys, key)
                return nil
            })
        })
        if err != nil {
            logging.Err("build %s: %v", l.index, err)
            continue
        }
        if len(keys) == 0 { continue }
        // Sorted before they are written, which is the whole cost of this: rows
        // come out of the source in address order, which is random against the
        // index key, and bbolt rebalances on every such insert. Measured on the
        // real exports, sorting first took the three builds from 3.45 s to
        // 106 ms.
        sort.Slice(keys, func(i, j int) bool { return string(keys[i]) < string(keys[j]) })
        err = db.Update(func(tx *bbolt.Tx) error {
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
        logging.Status("built %s: %d addresses in %s", l.index, len(keys), time.Since(started).Round(time.Millisecond))
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
