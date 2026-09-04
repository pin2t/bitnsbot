package main

import "encoding/json"
import "path/filepath"
import "strings"
import "testing"
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
