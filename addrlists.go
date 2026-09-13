package main

import "strconv"

import "bitnsbot/app"
import "bitnsbot/logging"

// The three ranked address lists the Mini App's Addresses tab shows: how busy an
// address is, what it holds now, and how long its coins have sat still. All three
// are three readings of one row of the addrstat table — the statistics that
// package gathers — and all three are an `order by` over it, served by the index
// on the column each ranks.
//
// That is the whole of the ranking now. Under bbolt this file kept three index
// buckets of its own, rebuilt hourly, because a bucket keyed by address cannot be
// walked in value order; SQLite has an index for that, and `addrstat_balance`,
// `addrstat_txs` and `addrstat_last` are it. The rebuild goroutine, the key
// packing and the signal that drove them are gone with it.
type addrList struct {
    kind string
    // column is what the list ranks by, and order the end it starts from: the
    // busiest address and the largest balance rank first, where the oldest date
    // does.
    column string
    order  string
    // keep is which rows belong in the list at all. A record the scan has not
    // reached is all zeroes — and an address that has never been paid is not the
    // poorest address on the chain, nor has one holding nothing abandoned any
    // coins.
    keep string
}

// A slice rather than a map, so the order they are tried in is fixed.
var addrLists = []addrList{
    {"active", "txs", "desc", "txs > 0"},
    {"rich", "balance", "desc", "balance > 0"},
    {"abandoned", "last", "asc", "balance > 0 and last > 0"},
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

// Addresses reads one window of one ranked list: an `order by` on the column that
// list ranks, `limit`ed to the batch and `offset` by how far down the reader has
// scrolled. The index on that column is what makes it a walk of the ranking rather
// than a sort of the table.
//
// **Nothing breaks a tie, deliberately.** Values collide heavily — 120 119 active
// addresses hold 14 269 distinct counts — and paging needs equal values to come
// back in the same order every time, or a batch boundary inside a group would
// repeat or skip a row. An `order by txs desc, addr` says that in SQL and costs a
// temp B-tree: measured at row 9000 of the real data, **8.7 ms against 0.2 ms**,
// because the index is on the one column and the second term has to be sorted.
// The index entry is (value, rowid), so what it yields inside a group is rowid
// order — stable for a row that stays put, which is every row here: the collector
// updates rows and never re-inserts them. So the order is the index's rather than
// the query's, and TestAppAddressListRanksCollidingValues pins that paging through
// a group of equal values sees each of them once.
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
    // One row more than is wanted, which is how "is there another batch below
    // this one" is answered without counting the table.
    var rows, err = db.Query("select addr, "+list.column+" from addrstat where "+list.keep+
        " order by "+list.column+" "+list.order+" limit ? offset ?", want+1, rng.From)
    if err != nil {
        logging.Warn("mini app: %s addresses: %v", rng.Kind, err)
        return out
    }
    defer rows.Close()
    for rows.Next() {
        if len(out.Rows) == want {
            out.More = true
            break
        }
        var addr string
        var n int64
        if err := rows.Scan(&addr, &n); err != nil { continue }
        var value string
        switch list.kind {
        case "active":
            value = i18nl(lang).Sprintf("%s txs", group(n))
        case "rich":
            // The same shape btcAmount gives, without its USD tail: a list row
            // has one column for this, and the price belongs on the details
            // page. Under a whole coin the satoshi are kept, or every small
            // balance would render as "0 BTC".
            if n >= 1e8 {
                value = strconv.FormatFloat(toBTC(n), 'f', 2, 64) + " BTC"
            } else {
                value = trimZeros(strconv.FormatFloat(toBTC(n), 'f', 8, 64)) + " BTC"
            }
        case "abandoned":
            value = day(n, lang)
        }
        out.Rows = append(out.Rows, app.Addr{Short: short(addr), Id: addr,
            Value: value, Idx: rng.From + len(out.Rows)})
    }
    out.Next = rng.From + len(out.Rows)
    if out.Next >= addrsMaxRows { out.More = false }
    out.OK = len(out.Rows) > 0
    return out
}
