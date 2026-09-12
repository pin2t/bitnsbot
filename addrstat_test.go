package main

import "context"
import "encoding/json"
import "path/filepath"
import "strings"
import "testing"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"
import "bitnsbot/addrstat"

// emptyChain is a chain with one block that moves nothing: enough for Collect to
// reach the tip, which is what makes the records answerable.
type emptyChain struct{}

func (emptyChain) Tip(ctx context.Context) (int, error) { return 0, nil }

func (emptyChain) BlockAt(ctx context.Context, height int) (addrindex.Block, error) {
    return addrindex.Block{Hash: "empty", Raw: make([]byte, 81), Spent: []byte{0}}, nil
}

// seedAddrStat puts one address in the rich list, opens the database so the
// collector picks it up, writes the record it would have gathered, and runs a
// pass so the scan counts as complete.
func seedAddrStat(t *testing.T, addr string, s addrstat.Stat) {
    t.Helper()
    if err := openDB(filepath.Join(t.TempDir(), "bitnsbot.db")); err != nil { t.Fatalf("openDB: %v", err) }
    t.Cleanup(func() { closeDB() })
    if err := db.Update(func(tx *bbolt.Tx) error {
        var b, berr = tx.CreateBucketIfNotExists([]byte("rich"))
        if berr != nil { return berr }
        return b.Put([]byte(addr), []byte("1"))
    }); err != nil { t.Fatal(err) }
    if err := addrstat.Init(db); err != nil { t.Fatalf("addrstat init: %v", err) }
    var data, err = json.Marshal(s)
    if err != nil { t.Fatal(err) }
    if err := db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("addrstat")).Put([]byte(addr), data)
    }); err != nil { t.Fatal(err) }
    if err := addrstat.Collect(emptyChain{}); err != nil { t.Fatalf("collect: %v", err) }
}

// An address the collector follows is answered from its record — with no node at
// all, which is what the stored statistics are for.
func TestAddressAnsweredFromStatistics(t *testing.T) {
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    seedAddrStat(t, addr, addrstat.Stat{
        Type: "p2pkh", Balance: 9990000, Recv: 10000000, Sent: 10000, Flow: 10010000,
        Fees: 5000, Txs: 4321, First: 1231006505, Last: 1700000000,
    })
    var saved = core
    core = nil // the answer must not need one
    t.Cleanup(func() { core = saved })
    var pairs, valid, err = addrPairs(context.Background(), "", addr)
    if err != nil || !valid { t.Fatalf("addrPairs: valid=%v err=%v", valid, err) }
    var got = map[string]string{}
    for _, p := range pairs { got[p[0]] = p[1] }
    for label, want := range map[string]string{
        "Type":           "standard (P2PKH)",
        "Balance":        "0.0999 BTC",
        "Total received": "0.1 BTC",
        "Total sent":     "10 000 sats",
        "Transactions":   "4 321",
    } {
        if got[label] != want { t.Errorf("%s = %q, want %q", label, got[label], want) }
    }
    // the count carries no trailing "+": this history is whole, where the live
    // path's is capped
    if strings.Contains(got["Transactions"], "+") { t.Error("a gathered count was marked partial") }
    if got["First tx"] == "" || got["Last tx"] == "" || got["Activity period"] == "" {
        t.Errorf("dates missing: %v", got)
    }
}

// The Mini App reads the same records through the same builder, so its address
// page answers without a node too.
func TestAppAddressPageUsesStatistics(t *testing.T) {
    var addr = "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"
    seedAddrStat(t, addr, addrstat.Stat{Type: "segwit", Balance: 100000000, Recv: 100000000, Txs: 7})
    var saved = core
    core = nil
    t.Cleanup(func() { core = saved })
    var info = appSource{}.AddrInfo("", addr)
    if !info.OK { t.Fatal("the app got nothing for a gathered address") }
    var labels []string
    for _, f := range info.Rows { labels = append(labels, f.Label+"="+f.Value) }
    var joined = strings.Join(labels, " ")
    if !strings.Contains(joined, "Type=segwit (bech32)") || !strings.Contains(joined, "Balance=1.00 BTC") {
        t.Errorf("rows = %v", labels)
    }
}
