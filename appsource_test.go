package main

import "errors"
import "fmt"
import "path/filepath"
import "strings"
import "testing"
import "bitnsbot/app"
import "bitnsbot/cursors"
import "bitnsbot/rates"
import "bitnsbot/txwatches"
import "bitnsbot/watches"

// The Mini App's Watches tab is the only per-user thing the app serves, and this
// is the line that scopes it. The app package's tests run against a fake Source,
// so without this one nothing would catch the filter being dropped.
func TestAppWatchesAreScopedToTheChat(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    txwatches.Reset()
    if err := watches.Add(42, "bc1qmine", "John"); err != nil { t.Fatalf("add: %v", err) }
    if err := watches.Add(99, "bc1qtheirs", ""); err != nil { t.Fatalf("add: %v", err) }
    txwatches.Add(strings.Repeat("a", 64), 42, "mine")
    txwatches.Add(strings.Repeat("b", 64), 99, "theirs")

    var mine = appSource{}.Watches(42)
    if !mine.OK { t.Fatal("lookup failed") }
    if len(mine.Addresses) != 1 || mine.Addresses[0].Id != "bc1qmine" {
        t.Errorf("addresses = %+v; want only this chat's", mine.Addresses)
    }
    if mine.Addresses[0].Alias != "John" {
        t.Errorf("alias = %q, want John", mine.Addresses[0].Alias)
    }
    if len(mine.Txs) != 1 || mine.Txs[0].Id != strings.Repeat("a", 64) {
        t.Errorf("transactions = %+v; want only this chat's", mine.Txs)
    }
    // the short form is what the row shows, the full id is what its link carries
    if mine.Txs[0].Short != short(strings.Repeat("a", 64)) {
        t.Errorf("Short = %q, want the shortened id", mine.Txs[0].Short)
    }

    var theirs = appSource{}.Watches(99)
    if len(theirs.Addresses) != 1 || theirs.Addresses[0].Id != "bc1qtheirs" {
        t.Errorf("the other chat got %+v", theirs.Addresses)
    }

    // a chat watching nothing gets an empty list, not everyone else's
    var none = appSource{}.Watches(7)
    if len(none.Addresses) != 0 || len(none.Txs) != 0 {
        t.Errorf("an unrelated chat was served %+v / %+v", none.Addresses, none.Txs)
    }
    if !none.OK {
        t.Error("watching nothing is not a failure")
    }
}

// The Mini App's block, transaction, address and miner sections are the bot's
// own lines, and the reader's language reaches them as an argument rather than
// through a chat — a page opened from the webview has no chat behind it. Only
// main can catch a regression here: the app package's tests run against a fake
// Source, which has no words of its own to translate.
func TestAppSectionsAreTranslated(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    if err := storeBlock(&blockInfo{Height: 700001, Hash: "0000000000000000000abc", Time: 1700000000,
        Size: 1500000, NumTx: 4000, Miner: "AntPool", Difficulty: 9e13,
        Reward: 312500000, Total: 320000000}); err != nil {
        t.Fatalf("store block: %v", err)
    }
    if err := storeBlock(&blockInfo{Height: 700002, Hash: "0000000000000000000abd", NumTx: 2100}); err != nil {
        t.Fatalf("store block: %v", err)
    }
    var labels = func(i app.Info) string {
        var out []string
        for _, r := range i.Rows { out = append(out, r.Label+"="+r.Value) }
        return strings.Join(out, "\n")
    }
    var en = appSource{}.BlockInfo("", 700001)
    if en.Title != "Block 700 001" || !strings.Contains(labels(en), "Hash=") {
        t.Errorf("English block page:\n%s\n%s", en.Title, labels(en))
    }
    var ru = appSource{}.BlockInfo("ru", 700001)
    if ru.Title != "Блок 700 001" {
        t.Errorf("block title = %q, want it translated", ru.Title)
    }
    for _, want := range []string{"Хеш=", "Награда=", "Майнер=AntPool"} {
        if !strings.Contains(labels(ru), want) {
            t.Errorf("block page is missing %q:\n%s", want, labels(ru))
        }
    }
    // the values are translated too, not only the labels: a size carries its
    // unit and an unknown fee distribution says so in words
    for _, want := range []string{"1.5 МБ", "недоступно"} {
        if !strings.Contains(labels(ru), want) {
            t.Errorf("block page value %q is still English:\n%s", want, labels(ru))
        }
    }
    if strings.Contains(labels(ru), "Hash=") {
        t.Errorf("an English label survived translation:\n%s", labels(ru))
    }
    // the list of blocks: main owns the "Unknown" placeholder and the row's
    // transaction count, so neither reaches the page through the template
    var list = appSource{}.Blocks("ru", app.Range{})
    if len(list.Rows) != 2 { t.Fatalf("block list has %d rows, want 2", len(list.Rows)) }
    if list.Rows[0].Miner != "неизвестен" || list.Rows[0].MinerKnown {
        t.Errorf("unattributed miner = %q, want it translated", list.Rows[0].Miner)
    }
    if list.Rows[0].Txs != "2 100 тр." {
        t.Errorf("transaction count = %q, want it translated", list.Rows[0].Txs)
    }
    if got := (appSource{}).Blocks("", app.Range{}).Rows[0].Txs; got != "2 100 txs" {
        t.Errorf("English transaction count = %q", got)
    }
    // the miner page
    if _, err := db.Exec(`insert into miners (name, address, tag, blocks, reward, fees, totalWork,
        lastWork) values ('AntPool', '', '', 6, 1950000000, 40000000, 3.6e24, 6.0e23)`); err != nil {
        t.Fatalf("seed miner stats: %v", err)
    }
    var mtx, merr = db.Begin()
    if merr != nil { t.Fatal(merr) }
    if err := cursors.Set(mtx, cursors.Miners, 9); err != nil { t.Fatal(err) }
    if err := mtx.Commit(); err != nil { t.Fatal(err) }
    var miner = appSource{}.MinerInfo("ru", "AntPool")
    if !miner.OK || !strings.Contains(labels(miner), "Блоков добыто=6 блоков") {
        t.Errorf("miner page:\n%s", labels(miner))
    }
    // the market card's periods, which main names rather than the page
    rates.Add(65000)
    var market = appSource{}.Market("ru")
    if len(market.Changes) == 0 || market.Changes[0].Label != "1д" {
        t.Errorf("market periods = %+v, want them translated", market.Changes)
    }
    if got := (appSource{}).Market("").Changes[0].Label; got != "1d" {
        t.Errorf("English market period = %q", got)
    }
}


// The block page's hash is tappable, and it is the whole row — the value is one
// id, so there is nothing plain to keep around it.
func TestBlockHashIsLinked(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    var hash = "0000000000000000000209d0dbbd5a37b0e0e0a2f8a1ba36d6f4f0e9c0b1a2f3"
    if err := storeBlock(&blockInfo{Height: 700001, Hash: hash, Time: 1700000000,
        Size: 1500000, NumTx: 4000, Miner: "AntPool", Difficulty: 9e13,
        Reward: 312500000, Total: 320000000}); err != nil {
        t.Fatalf("store block: %v", err)
    }
    var info = appSource{}.BlockInfo("", 700001)
    if !info.OK || len(info.Rows) == 0 { t.Fatal("no block page") }
    var row = info.Rows[0]
    if row.Value != short(hash) {
        t.Fatalf("row 0 = %q, want the hash — the rest of this test reads it", row.Value)
    }
    if len(row.Parts) != 1 || row.Parts[0].Id != hash || row.Parts[0].Text != short(hash) {
        t.Errorf("hash row parts = %#v, want the whole value linked to %s", row.Parts, hash)
    }
    // and a row that mentions no id is left whole
    for _, r := range info.Rows[1:] {
        if r.Label == "Time" && r.Parts != nil {
            t.Errorf("the time row was cut up: %#v", r.Parts)
        }
    }
}

// splitLinks is what keeps a line readable while making the ids in it tappable:
// the text between them survives, in order, and a line with none is left alone.
func TestSplitLinks(t *testing.T) {
    var links = []linked{{"#963268", "963268"}, {"1A1zP1...DivfNa", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}}
    var got = splitLinks("412 (block #963268)", links)
    var want = []app.Part{{Text: "412 (block "}, {Text: "#963268", Id: "963268"}, {Text: ")"}}
    if len(got) != len(want) {
        t.Fatalf("parts = %#v, want %#v", got, want)
    }
    for i := range want {
        if got[i] != want[i] { t.Errorf("part %d = %#v, want %#v", i, got[i], want[i]) }
    }
    // two ids in one line come out in reading order, with the separator between
    var two = splitLinks("1A1zP1...DivfNa, 1A1zP1...DivfNa", links)
    if len(two) != 3 || two[1].Text != ", " || two[0].Id != two[2].Id {
        t.Errorf("parts = %#v, want both mentions linked with the comma between", two)
    }
    if splitLinks("9 990 000 sats (≈ $6,614)", links) != nil {
        t.Error("a line mentioning no id should be left whole")
    }
    if splitLinks("anything", nil) != nil {
        t.Error("no ids means nothing to cut")
    }
}


// The transaction page through the real builders: the block it confirmed in and
// the addresses on either side are tappable, and the text around them survives.
func TestTxInfoLinksBlockAndAddresses(t *testing.T) {
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var blockHash = "0000000000000000000209d0dbbd5a37b0e0e0a2f8a1ba36d6f4f0e9c0b1a2f3"
    var from = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var to = "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        switch method {
        case "getblockheader":
            // only the block hash has a header; a txid must not look like a block
            if id, _ := params[0].(string); id == blockHash {
                return map[string]any{"height": 700001}, nil
            }
            return nil, errors.New("Block not found")
        case "getrawtransaction":
            return map[string]any{
                "txid": txid, "confirmations": 412, "time": 1700000000, "blockhash": blockHash,
                "size": 223, "vsize": 141, "fee": 0.0000141,
                "vin": []map[string]any{{"txid": "prev", "vout": 0,
                    "prevout": map[string]any{"value": 0.1, "scriptPubKey": map[string]any{"address": from}}}},
                "vout": []map[string]any{{"value": 0.0999, "n": 0,
                    "scriptPubKey": map[string]any{"address": to}}},
            }, nil
        }
        return nil, nil
    })
    defer srv.Close()
    core = newFakeCoreConn(t, srv)
    defer func() { core = nil }()
    var info = appSource{}.TxInfo("", txid)
    if !info.OK { t.Fatal("no transaction page") }
    var linked = map[string]string{}
    for _, r := range info.Rows {
        for _, part := range r.Parts {
            if part.Id != "" { linked[part.Id] = part.Text }
        }
    }
    for id, text := range map[string]string{"700001": "#700001", from: short(from), to: short(to)} {
        if linked[id] != text {
            t.Errorf("%s is linked as %q, want %q — all links: %v", id, linked[id], text, linked)
        }
    }
    // the block hash is in the bot's button list but is nowhere in the text, so
    // it must not turn up as a link with nothing to attach to
    if _, ok := linked[blockHash]; ok {
        t.Errorf("the block hash was linked though the page never shows it: %v", linked)
    }
    // and each cut-up row still reads exactly as the bot's line does
    for _, r := range info.Rows {
        if r.Parts == nil { continue }
        var joined string
        for _, part := range r.Parts { joined += part.Text }
        if joined != r.Value {
            t.Errorf("row %q reassembles to %q, want %q", r.Label, joined, r.Value)
        }
    }
}


// The same bug from main's side: TxInfo hands back a block for an id the node
// has a header for, and that page has to say it is a block — the app has no
// other way to know, and would otherwise offer to watch it as a transaction.
func TestTxInfoOnABlockHashIsABlockPage(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil { t.Fatal(err) }
    defer closeDB()
    var hash = "0000000000000000000209d0dbbd5a37b0e0e0a2f8a1ba36d6f4f0e9c0b1a2f3"
    if err := storeBlock(&blockInfo{Height: 700001, Hash: hash, Time: 1700000000,
        Size: 1500000, NumTx: 4000, Miner: "AntPool", Reward: 312500000, Total: 320000000}); err != nil {
        t.Fatal(err)
    }
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        if method == "getblockheader" {
            if id, _ := params[0].(string); id == hash { return map[string]any{"height": 700001}, nil }
        }
        return nil, errors.New("Block not found")
    })
    defer srv.Close()
    core = newFakeCoreConn(t, srv)
    defer func() { core = nil }()
    var info = appSource{}.TxInfo("", hash)
    if !info.OK || info.Title != "Block 700 001" {
        t.Fatalf("a block hash should open the block page, got %#v", info.Title)
    }
    if info.Kind != "block" {
        t.Errorf("Kind = %q, want block — this is what keeps the watch button off it", info.Kind)
    }
    // and the block endpoint says the same thing about the same page
    if got := (appSource{}).BlockInfo("", 700001); got.Kind != "block" {
        t.Errorf("BlockInfo Kind = %q, want block", got.Kind)
    }
}

// seedAddrList gives each address the figure the named list ranks it by, merging
// into the row the other two lists read. There is nothing to build afterwards:
// the ranking is an `order by` served by the index on that column.
func seedAddrList(t *testing.T, kind string, rows map[string]int64) {
    t.Helper()
    for addr, n := range rows {
        var _, err = db.Exec(`insert into addrstat (addr, type, balance, recv, sent, flow, fees, txs,
            first, last) values (?, '', 0, 0, 0, 0, 0, 0, 0, 0) on conflict(addr) do nothing`, addr)
        if err != nil { t.Fatalf("seed %s: %v", kind, err) }
        var col = map[string]string{"active": "txs", "rich": "balance", "abandoned": "last"}[kind]
        if _, err := db.Exec("update addrstat set "+col+" = ? where addr = ?", n, addr); err != nil {
            t.Fatalf("seed %s: %v", kind, err)
        }
        // the abandoned ranking is of coins that have not moved, so an address
        // has to hold some to be in it at all
        if kind == "abandoned" {
            if _, err := db.Exec("update addrstat set balance = max(balance, 1) where addr = ?", addr); err != nil {
                t.Fatalf("seed %s: %v", kind, err)
            }
        }
    }
}

// The three lists are ranked by their value, which the bucket is not ordered by
// — it is keyed by address. This is the line that does the ranking, and it is
// only reachable from main: the app package's tests page a list that is already
// in order.
func TestAppAddressListsAreRanked(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    // deliberately in an order the bucket's own key order would not produce
    seedAddrList(t, "active", map[string]int64{"aaa": 10, "bbb": 9000, "ccc": 300})
    seedAddrList(t, "rich", map[string]int64{"aaa": 100000000, "bbb": 2500000000000, "ccc": 9990000})
    seedAddrList(t, "abandoned", map[string]int64{"aaa": 1500000000, "bbb": 1233636834, "ccc": 1400000000})
    var cases = []struct {
        kind  string
        order []string
        first string
    }{
        {"active", []string{"bbb", "ccc", "aaa"}, "9 000 txs"},
        {"rich", []string{"bbb", "aaa", "ccc"}, "25000.00 BTC"},
        // ascending: the oldest last transaction is the most abandoned
        {"abandoned", []string{"bbb", "ccc", "aaa"}, "3 february 2009"},
    }
    for _, c := range cases {
        var got = appSource{}.Addresses("", app.AddrRange{Kind: c.kind})
        if !got.OK || len(got.Rows) != len(c.order) {
            t.Fatalf("%s: got %d rows, want %d (%+v)", c.kind, len(got.Rows), len(c.order), got)
        }
        for i, want := range c.order {
            if got.Rows[i].Id != want {
                t.Errorf("%s: row %d is %q, want %q", c.kind, i, got.Rows[i].Id, want)
            }
            if got.Rows[i].Idx != i {
                t.Errorf("%s: row %d carries offset %d", c.kind, i, got.Rows[i].Idx)
            }
        }
        if got.Rows[0].Value != c.first {
            t.Errorf("%s: top row reads %q, want %q", c.kind, got.Rows[0].Value, c.first)
        }
        if got.More {
            t.Errorf("%s: a list shorter than a batch has nothing more to fetch", c.kind)
        }
    }
    // a balance under a whole coin keeps its satoshi rather than rounding away
    var rich = appSource{}.Addresses("", app.AddrRange{Kind: "rich"})
    if rich.Rows[2].Value != "0.0999 BTC" {
        t.Errorf("small balance reads %q, want 0.0999 BTC", rich.Rows[2].Value)
    }
}

// Paging is by offset, which is exact because these lists never grow at the
// head: the batches must partition the ranking with no row repeated or skipped.
func TestAppAddressListPages(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    var vals = map[string]int{}
    var rows = map[string]int64{}
    for i := 0; i < 40; i++ {
        var addr = string(rune('a'+i/26)) + string(rune('a'+i%26))
        vals[addr] = 1000 - i
        rows[addr] = int64(vals[addr])
    }
    seedAddrList(t, "active", rows)
    var seen []string
    var from, batches int
    for {
        var got = appSource{}.Addresses("", app.AddrRange{Kind: "active", From: from})
        if batches == 0 && len(got.Rows) != addrsFirstPage {
            t.Fatalf("first batch has %d rows, want %d", len(got.Rows), addrsFirstPage)
        }
        if batches > 0 && got.More && len(got.Rows) != addrsPage {
            t.Fatalf("batch %d has %d rows, want %d", batches, len(got.Rows), addrsPage)
        }
        for _, r := range got.Rows { seen = append(seen, r.Id) }
        batches++
        if !got.More { break }
        if got.Next <= from { t.Fatalf("batch %d did not advance past %d", batches, from) }
        from = got.Next
    }
    if len(seen) != 40 {
        t.Fatalf("paged %d rows over %d batches, want 40", len(seen), batches)
    }
    // strictly descending, so nothing was repeated or skipped across batches
    for i := 1; i < len(seen); i++ {
        if vals[seen[i-1]] <= vals[seen[i]] {
            t.Fatalf("rows %d and %d are out of order: %s (%d) then %s (%d)",
                i-1, i, seen[i-1], vals[seen[i-1]], seen[i], vals[seen[i]])
        }
    }
    // and a restored list reaches the row Back returns to, in one batch
    var back = appSource{}.Addresses("", app.AddrRange{Kind: "active", Down: 22, Restore: true})
    if len(back.Rows) != 23 || back.Rows[22].Idx != 22 {
        t.Errorf("restored list has %d rows, want 23 reaching offset 22", len(back.Rows))
    }
    if back.Rows[22].Id != seen[22] {
        t.Errorf("restored row 22 is %q, want %q — the same row the scroll reached", back.Rows[22].Id, seen[22])
    }
}

// Values collide heavily on real data — 120 119 active addresses hold 14 269
// distinct counts — so a ranking has to show every address that shares a value,
// in an order that does not change between batches. The `, addr` on the end of
// the ORDER BY is what makes the tie deterministic; without it, paging could
// repeat or skip a row inside a group.
func TestAppAddressListRanksCollidingValues(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    var rows = map[string]int64{}
    for i := 0; i < 8; i++ {
        rows[fmt.Sprintf("addr%02d", i)] = int64(500 + i%2)
    }
    seedAddrList(t, "active", rows)
    var got = appSource{}.Addresses("", app.AddrRange{Kind: "active"})
    if len(got.Rows) != 8 {
        t.Fatalf("the list shows %d of 8 addresses: %+v", len(got.Rows), got.Rows)
    }
    var seen = map[string]int{}
    for i, r := range got.Rows {
        seen[r.Id]++
        if i < 4 && r.Value != "501 txs" { t.Errorf("row %d is %q, want the higher value first", i, r.Value) }
        if i >= 4 && r.Value != "500 txs" { t.Errorf("row %d is %q, want the lower value last", i, r.Value) }
    }
    for addr, c := range seen {
        if c != 1 { t.Errorf("%s appears %d times", addr, c) }
    }
    // and paging through the same ranking a row at a time visits each once
    var page []string
    for from := 0; from < 8; from++ {
        var batch = appSource{}.Addresses("", app.AddrRange{Kind: "active", From: from})
        if len(batch.Rows) == 0 { break }
        page = append(page, batch.Rows[0].Id)
    }
    for i, id := range page {
        if id != got.Rows[i].Id {
            t.Errorf("row %d is %s paged and %s in one batch — the tie-break is not stable", i, id, got.Rows[i].Id)
        }
    }
}

// An absent bucket is a bot whose lists were never imported, and an unknown kind
// can only come from an edited URL. Neither is an error, and neither may serve
// another list's rows.
func TestAppAddressListMissing(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    seedAddrList(t, "active", map[string]int64{"aaa": 10})
    for _, kind := range []string{"rich", "abandoned", "nonesuch", ""} {
        var got = appSource{}.Addresses("", app.AddrRange{Kind: kind})
        if got.OK || len(got.Rows) != 0 {
            t.Errorf("kind %q served %+v", kind, got.Rows)
        }
        if got.Kind != kind {
            t.Errorf("kind %q came back as %q", kind, got.Kind)
        }
    }
}

// A restore renders every row into one response, where a scroll delivers them a
// batch at a time — so however deep the reader had scrolled, Back is bounded
// separately. Without this an edited down= is a request for a megabyte of rows.
func TestAppAddressListBoundsARestore(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    var rows = map[string]int64{}
    for i := 0; i < addrsRestoreRows + 50; i++ {
        rows[fmt.Sprintf("a%05d", i)] = int64(100000 - i)
    }
    seedAddrList(t, "rich", rows)
    var got = appSource{}.Addresses("", app.AddrRange{Kind: "rich", Down: addrsRestoreRows + 40, Restore: true})
    if len(got.Rows) != addrsRestoreRows {
        t.Errorf("a restore returned %d rows, want it capped at %d", len(got.Rows), addrsRestoreRows)
    }
    // and it still continues from where it stopped rather than claiming the end
    if !got.More || got.Next != addrsRestoreRows {
        t.Errorf("capped restore says Next=%d More=%v; the scroll must carry on", got.Next, got.More)
    }
}
