package main

import "bytes"
import "encoding/binary"
import "encoding/json"
import "errors"
import "fmt"
import "path/filepath"
import "strconv"
import "strings"
import "testing"
import "time"
import "bitnsbot/app"
import "bitnsbot/rates"
import "bitnsbot/txwatches"
import "bitnsbot/watches"
import "go.etcd.io/bbolt"

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
    if err := db.Update(func(tx *bbolt.Tx) error {
        var data, err = json.Marshal(map[string]any{"Blocks": 6, "Reward": 1950000000,
            "Fees": 40000000, "Work": 3.6e24, "LastWork": 6.0e23})
        if err != nil { return err }
        if err := tx.Bucket([]byte("miners-stat")).Put([]byte("AntPool"), data); err != nil { return err }
        return tx.Bucket([]byte("miners-cursor")).Put([]byte("cursor"), []byte("9"))
    }); err != nil {
        t.Fatalf("seed miner stats: %v", err)
    }
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

// seedAddrBucket fills one of the three ranked buckets the way tools/csvimport
// does — the address as the key, the figure as decimal text — and then builds
// the index, which is what startup does and what every read goes through.
func seedAddrBucket(t *testing.T, name string, rows map[string]string) {
    t.Helper()
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b, berr = tx.CreateBucketIfNotExists([]byte(name))
        if berr != nil { return berr }
        for k, v := range rows {
            if err := b.Put([]byte(k), []byte(v)); err != nil { return err }
        }
        return nil
    })
    if err != nil { t.Fatalf("seed %s: %v", name, err) }
    buildAddrIndexes()
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
    seedAddrBucket(t, "active", map[string]string{"aaa": "10", "bbb": "9000", "ccc": "300"})
    seedAddrBucket(t, "rich", map[string]string{"aaa": "100000000", "bbb": "2500000000000", "ccc": "9990000"})
    seedAddrBucket(t, "abandoned", map[string]string{"aaa": "1500000000", "bbb": "1233636834", "ccc": "1400000000"})
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
    var rows = map[string]string{}
    for i := 0; i < 40; i++ {
        var addr = string(rune('a'+i/26)) + string(rune('a'+i%26))
        vals[addr] = 1000 - i
        rows[addr] = strconv.Itoa(vals[addr])
    }
    seedAddrBucket(t, "active", rows)
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

// A value this cannot read is skipped the way a block record that fails to
// decode is, rather than ending the scan and truncating the ranking.
func TestAppAddressListSkipsUnreadableRows(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    seedAddrBucket(t, "active", map[string]string{
        "aaa": "10", "bad": "hex:0000000000000064", "bbb": "9000", "zzz": "", "ccc": "300",
    })
    var got = appSource{}.Addresses("", app.AddrRange{Kind: "active"})
    if len(got.Rows) != 3 {
        t.Fatalf("got %d rows, want the 3 readable ones: %+v", len(got.Rows), got.Rows)
    }
    if got.Rows[0].Id != "bbb" || got.Rows[2].Id != "aaa" {
        t.Errorf("the readable rows are misordered: %+v", got.Rows)
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
    seedAddrBucket(t, "active", map[string]string{"aaa": "10"})
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
    var rows = map[string]string{}
    for i := 0; i < addrsRestoreRows + 50; i++ {
        rows[fmt.Sprintf("a%05d", i)] = strconv.Itoa(100000 - i)
    }
    seedAddrBucket(t, "rich", rows)
    var got = appSource{}.Addresses("", app.AddrRange{Kind: "rich", Down: addrsRestoreRows + 40, Restore: true})
    if len(got.Rows) != addrsRestoreRows {
        t.Errorf("a restore returned %d rows, want it capped at %d", len(got.Rows), addrsRestoreRows)
    }
    // and it still continues from where it stopped rather than claiming the end
    if !got.More || got.Next != addrsRestoreRows {
        t.Errorf("capped restore says Next=%d More=%v; the scroll must carry on", got.Next, got.More)
    }
}

// The index key is the value with the address appended, and the address half is
// load-bearing: values collide heavily on real data — 120 119 active addresses
// hold 14 269 distinct counts — so a bare value key would keep one address per
// value and silently drop the rest.
func TestAddrIndexKeepsCollidingValues(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    // eight addresses, two distinct values between them
    var rows = map[string]string{}
    for i := 0; i < 8; i++ {
        rows[fmt.Sprintf("addr%02d", i)] = strconv.Itoa(500 + i%2)
    }
    seedAddrBucket(t, "active", rows)
    var n int
    db.View(func(tx *bbolt.Tx) error {
        n = tx.Bucket([]byte("activeindex")).Stats().KeyN
        return nil
    })
    if n != 8 {
        t.Fatalf("the index holds %d entries for 8 addresses; colliding values were overwritten", n)
    }
    var got = appSource{}.Addresses("", app.AddrRange{Kind: "active"})
    if len(got.Rows) != 8 {
        t.Fatalf("the list shows %d of 8 addresses: %+v", len(got.Rows), got.Rows)
    }
    // the four highest come first, and every address appears exactly once
    var seen = map[string]int{}
    for i, r := range got.Rows {
        seen[r.Id]++
        if i < 4 && r.Value != "501 txs" { t.Errorf("row %d is %q, want the higher value first", i, r.Value) }
        if i >= 4 && r.Value != "500 txs" { t.Errorf("row %d is %q, want the lower value last", i, r.Value) }
    }
    for addr, c := range seen {
        if c != 1 { t.Errorf("%s appears %d times", addr, c) }
    }
}

// The index is rebuilt from its bucket, from scratch, so a bucket replaced
// wholesale by tools/csvimport is picked up whole — rows added, rows gone, and a
// bucket emptied altogether.
func TestAddrIndexRebuildsFromItsBucket(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    seedAddrBucket(t, "rich", map[string]string{"aaa": "100000000"})
    // a row added behind the index's back is picked up by the next rebuild
    db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("rich")).Put([]byte("bbb"), []byte("900000000"))
    })
    buildAddrIndexes()
    var grown = appSource{}.Addresses("", app.AddrRange{Kind: "rich"})
    if len(grown.Rows) != 2 || grown.Rows[0].Id != "bbb" {
        t.Errorf("a rebuild should take the new row, got %+v", grown.Rows)
    }
    // and so is one that went away
    db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("rich")).Delete([]byte("bbb"))
    })
    buildAddrIndexes()
    var shrunk = appSource{}.Addresses("", app.AddrRange{Kind: "rich"})
    if len(shrunk.Rows) != 1 || shrunk.Rows[0].Id != "aaa" {
        t.Errorf("a rebuild should drop the removed row, got %+v", shrunk.Rows)
    }
    // an emptied bucket leaves no index at all, not the last one built from it
    db.Update(func(tx *bbolt.Tx) error {
        tx.DeleteBucket([]byte("rich"))
        var _, err = tx.CreateBucket([]byte("rich"))
        return err
    })
    buildAddrIndexes()
    var gone = appSource{}.Addresses("", app.AddrRange{Kind: "rich"})
    if gone.OK || len(gone.Rows) != 0 {
        t.Errorf("an emptied bucket still serves %+v", gone.Rows)
    }
    var exists bool
    db.View(func(tx *bbolt.Tx) error {
        exists = tx.Bucket([]byte("richindex")) != nil
        return nil
    })
    if exists {
        t.Error("a stale index outlived the rows it was built from")
    }
}

// The rebuild runs on its own goroutine: once at once, so a freshly imported
// database is not three empty lists until the first hour is up, then on the
// interval. The stop waits for a rebuild in flight, since shutdown closes the
// database right after it.
func TestAddrIndexRebuildsOnATicker(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    var old = addrIndexInterval
    addrIndexInterval = 20 * time.Millisecond
    defer func() { addrIndexInterval = old }()
    db.Update(func(tx *bbolt.Tx) error {
        var b, err = tx.CreateBucketIfNotExists([]byte("active"))
        if err != nil { return err }
        return b.Put([]byte("aaa"), []byte("10"))
    })
    var stop = startAddrIndexes()
    // the first build is immediate, not an interval away
    var listed bool
    for i := 0; i < 200 && !listed; i++ {
        listed = len(appSource{}.Addresses("", app.AddrRange{Kind: "active"}).Rows) == 1
        time.Sleep(5 * time.Millisecond)
    }
    if !listed {
        t.Fatal("the first rebuild never ran")
    }
    // a later change is taken on a tick, with nothing else prompting it
    db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("active")).Put([]byte("bbb"), []byte("99"))
    })
    var picked bool
    for i := 0; i < 200 && !picked; i++ {
        picked = len(appSource{}.Addresses("", app.AddrRange{Kind: "active"}).Rows) == 2
        time.Sleep(5 * time.Millisecond)
    }
    if !picked {
        t.Error("the ticker never rebuilt the index")
    }
    stop()
    // stop is what lets shutdown close the database safely, so it must return
    // rather than leaving the goroutine writing
    db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("active")).Put([]byte("ccc"), []byte("1"))
    })
    time.Sleep(60 * time.Millisecond)
    if n := len(appSource{}.Addresses("", app.AddrRange{Kind: "active"}).Rows); n != 2 {
        t.Errorf("the goroutine kept running after stop: %d rows", n)
    }
}

// A source bucket that is missing, or holds nothing this can read, is left
// without an index rather than with an empty one — an empty index would be
// "already there" and a later import would never be picked up.
func TestAddrIndexNotBuiltForNothing(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    seedAddrBucket(t, "abandoned", map[string]string{"aaa": "not a number"})
    buildAddrIndexes()
    var exists bool
    db.View(func(tx *bbolt.Tx) error {
        exists = tx.Bucket([]byte("abandonedindex")) != nil
        return nil
    })
    if exists {
        t.Error("an index was created for a bucket with nothing indexable in it")
    }
    // and once there is something to index, the next build takes it
    seedAddrBucket(t, "abandoned", map[string]string{"bbb": "1233636834"})
    var got = appSource{}.Addresses("", app.AddrRange{Kind: "abandoned"})
    if len(got.Rows) != 1 || got.Rows[0].Id != "bbb" {
        t.Errorf("a later import should be indexed, got %+v", got.Rows)
    }
}

// The abandoned index packs its date into four bytes. A value too wide would
// truncate and rank as something else entirely, so it is left out instead.
func TestAddrIndexSkipsOversizedValues(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    seedAddrBucket(t, "abandoned", map[string]string{
        "good": "1233636834", "huge": "4294967296", "later": "1500000000",
    })
    var got = appSource{}.Addresses("", app.AddrRange{Kind: "abandoned"})
    if len(got.Rows) != 2 {
        t.Fatalf("got %d rows, want the 2 that fit: %+v", len(got.Rows), got.Rows)
    }
    if got.Rows[0].Id != "good" || got.Rows[1].Id != "later" {
        t.Errorf("oldest first was not preserved: %+v", got.Rows)
    }
}

// The whole entry is the key — the value big-endian, then the address — and
// nothing is stored as the bbolt value. A read slices the address back out, so a
// key written any other way would come back as the wrong address rather than as
// an error.
func TestAddrIndexKeyFormat(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    defer closeDB()
    seedAddrBucket(t, "rich", map[string]string{"bc1qexample": "2500000000000"})
    seedAddrBucket(t, "abandoned", map[string]string{"1AbandonedOne": "1233636834"})
    var cases = []struct {
        index string
        want  []byte
    }{
        {"richindex", append(binary.BigEndian.AppendUint64(nil, 2500000000000), "bc1qexample"...)},
        {"abandonedindex", append(binary.BigEndian.AppendUint32(nil, 1233636834), "1AbandonedOne"...)},
    }
    for _, c := range cases {
        db.View(func(tx *bbolt.Tx) error {
            var k, v = tx.Bucket([]byte(c.index)).Cursor().First()
            if !bytes.Equal(k, c.want) {
                t.Errorf("%s key = %x, want %x", c.index, k, c.want)
            }
            if len(v) != 0 {
                t.Errorf("%s stores %q as its value; the key carries everything", c.index, v)
            }
            return nil
        })
    }
    // and the address the list shows is the one sliced back out of that key
    var got = appSource{}.Addresses("", app.AddrRange{Kind: "rich"})
    if len(got.Rows) != 1 || got.Rows[0].Id != "bc1qexample" {
        t.Errorf("the address did not survive the round trip: %+v", got.Rows)
    }
}
