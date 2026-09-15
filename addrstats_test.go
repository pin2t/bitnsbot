package main

import "testing"
import "bitnsbot/core"

// addressStats sums an address's history from resolved transactions. Under Core
// a confirmed transaction carries its own fee and its inputs' prevouts, so both
// the spending side and the fee read straight off it — no prevout fetching and
// no separate fee derivation, unlike the btcd path this replaces.
//
// received 1.0 BTC on 2015-01-01
//
// spent 1.0 on 2016-01-01: 0.9 out plus 0.0999 change back, fee 0.0001
//
// only the transaction the address actually spends from contributes a fee
func TestAddressStats(t *testing.T) {
    var addr = "addresswithhistory"
    var txs = []*core.Transaction{
        {
            Txid: "aa", Time: 1420070400,
            Vin:  []core.Vin{{Txid: "x", PrevOut: &core.PrevOut{Value: 1.0001, ScriptPubKey: core.ScriptPubKey{Address: "other"}}}},
            Vout: []core.Vout{{Value: 1.0, ScriptPubKey: core.ScriptPubKey{Address: addr}}},
        },
        {
            Txid: "bb", Time: 1451606400, Fee: 0.0001,
            Vin:  []core.Vin{{Txid: "aa", PrevOut: &core.PrevOut{Value: 1.0, ScriptPubKey: core.ScriptPubKey{Address: addr}}}},
            Vout: []core.Vout{
                {Value: 0.9, ScriptPubKey: core.ScriptPubKey{Address: "dest"}},
                {Value: 0.0999, ScriptPubKey: core.ScriptPubKey{Address: addr}},
            },
        },
    }
    var received, sent, fees, firstT, lastT = addressStats(txs, addr)
    if received != 109990000 {
        t.Fatalf("received = %v, want 109990000 sats (1.0 in, 0.0999 change back)", received)
    }
    if sent != 100000000 {
        t.Fatalf("sent = %v, want 100000000 sats", sent)
    }
    if fees != 10000 {
        t.Fatalf("fees = %v, want 10000 sats", fees)
    }
    if firstT != 1420070400 || lastT != 1451606400 {
        t.Fatalf("times = %d..%d, want 1420070400..1451606400", firstT, lastT)
    }
    if balance := received - sent; balance != 9990000 {
        t.Fatalf("balance = %v, want exactly 9990000 sats", balance)
    }
}
