package main

import "testing"

// addressStats sums an address's history from resolved transactions. Under Core
// a confirmed transaction carries its own fee and its inputs' prevouts, so both
// the spending side and the fee read straight off it — no prevout fetching and
// no separate fee derivation, unlike the btcd path this replaces.
func TestAddressStats(t *testing.T) {
    var addr = "addresswithhistory"
    var txs = []*coreTransaction{
        { // received 1.0 BTC on 2015-01-01
            Txid: "aa", Time: 1420070400,
            Vin:  []coreVin{{Txid: "x", PrevOut: &corePrevOut{Value: 1.0001, ScriptPubKey: coreScriptPubKey{Address: "other"}}}},
            Vout: []coreVout{{Value: 1.0, ScriptPubKey: coreScriptPubKey{Address: addr}}},
        },
        { // spent 1.0 on 2016-01-01: 0.9 out plus 0.0999 change back, fee 0.0001
            Txid: "bb", Time: 1451606400, Fee: 0.0001,
            Vin:  []coreVin{{Txid: "aa", PrevOut: &corePrevOut{Value: 1.0, ScriptPubKey: coreScriptPubKey{Address: addr}}}},
            Vout: []coreVout{
                {Value: 0.9, ScriptPubKey: coreScriptPubKey{Address: "dest"}},
                {Value: 0.0999, ScriptPubKey: coreScriptPubKey{Address: addr}},
            },
        },
    }
    var received, sent, fees, firstT, lastT = addressStats(txs, addr)
    if received != 1.0999 {
        t.Fatalf("received = %v, want 1.0999 (1.0 in, 0.0999 change back)", received)
    }
    if sent != 1.0 {
        t.Fatalf("sent = %v, want 1.0", sent)
    }
    // only the transaction the address actually spends from contributes a fee
    if fees != 0.0001 {
        t.Fatalf("fees = %v, want 0.0001", fees)
    }
    if firstT != 1420070400 || lastT != 1451606400 {
        t.Fatalf("times = %d..%d, want 1420070400..1451606400", firstT, lastT)
    }
    if balance := received - sent; balance < 0.0998 || balance > 0.1 {
        t.Fatalf("balance = %v, want ~0.0999", balance)
    }
}
