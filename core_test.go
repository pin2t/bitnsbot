package main

import "context"
import "testing"
import "bitnsbot/core"
import "bitnsbot/core/coretest"

// Verbosity 2 gives prevouts and a fee for a confirmed transaction, which is what
// lets txInputs skip fetching prevouts entirely.
func TestCoreGetRawTransactionPrevouts(t *testing.T) {
    coretest.Start(t, func(method string, params []interface{}) (interface{}, error) {
        if method != "getrawtransaction" {
            t.Fatalf("unexpected method: %s", method)
        }
        if len(params) < 2 {
            t.Fatalf("expected a verbosity argument, got %v", params)
        }
        if v, _ := params[1].(float64); v != 2 {
            t.Fatalf("verbosity = %v, want 2 (fee and prevouts)", params[1])
        }
        return map[string]any{
            "txid": "abc", "size": 225, "vsize": 200, "confirmations": 6, "fee": 0.0001,
            "vin": []map[string]any{{
                "txid": "prev", "vout": 0,
                "prevout": map[string]any{"value": 1.5, "scriptPubKey": map[string]any{"address": "bc1qsender"}},
            }},
            "vout": []map[string]any{{"value": 1.4999, "n": 0, "scriptPubKey": map[string]any{"address": "bc1qdest", "hex": "0014aa"}}},
        }, nil
    })
    var tx, err = core.GetRawTransaction(context.Background(), "abc")
    if err != nil {
        t.Fatalf("getRawTransaction: %v", err)
    }
    if tx.Vin[0].PrevOut == nil {
        t.Fatal("expected an inline prevout at verbosity 2")
    }
    var fee, addrs, spent, ok = txInputs(context.Background(), tx)
    if !ok {
        t.Fatal("txInputs failed on a transaction carrying its own prevouts")
    }
    if fee != 10000 {
        t.Fatalf("fee = %v, want the node's own 10000 sats", fee)
    }
    if len(addrs) != 1 || addrs[0] != "bc1qsender" {
        t.Fatalf("input addresses = %v, want [bc1qsender]", addrs)
    }
    if spent["bc1qsender"] != 150000000 {
        t.Fatalf("spent = %v, want 150000000 sats from bc1qsender", spent)
    }
}
