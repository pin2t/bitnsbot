package main

import "path/filepath"
import "strings"
import "testing"
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
