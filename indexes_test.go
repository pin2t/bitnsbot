package main

import "fmt"
import "path/filepath"
import "strconv"
import "strings"
import "sync"
import "testing"
import "bitnsbot/addrbal"
import "bitnsbot/core/coretest"
import "bitnsbot/cursors"

// One tip fetch, then each index to that tip, in order: the block cache, the
// miner statistics, the address index, the per-address statistics, the address
// balances. The fake node records every call, so the test pins the order by the
// sequence it sees: the single getblockcount first, the block cache's
// verbosity-2 getblocks all before the address index's verbosity-3 ones. The
// miner statistics have no pool definitions to attribute and addrstat no
// addresses to follow, so those two steps fetch nothing; addrbal stops six
// below the tip, which is below genesis here.
func TestUpdateIndexesRunsInOrder(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "indexes.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var mu sync.Mutex
    var calls []string
    coretest.Start(t, func(method string, params []interface{}) (interface{}, error) {
        var label = method
        if method == "getblock" {
            label = fmt.Sprintf("getblock:%v", params[1])
        }
        mu.Lock()
        calls = append(calls, label)
        mu.Unlock()
        switch method {
        case "getblockcount":
            return 2, nil
        case "getblockhash":
            return fmt.Sprintf("0000000000000000abc%v", params[0]), nil
        case "getblock":
            var height, _ = strconv.Atoi(strings.TrimPrefix(params[0].(string), "0000000000000000abc"))
            if params[1].(float64) == 3 {
                return map[string]any{"time": 1700000000,
                    "tx": []map[string]any{{"vin": []map[string]any{{"prevout": nil}},
                        "vout": []map[string]any{{"value": 50.0, "scriptPubKey": map[string]any{"hex": "51"}}}}}}, nil
            }
            return map[string]any{"hash": params[0], "height": height, "time": 1700000000, "size": 300,
                "tx": []map[string]any{{"txid": "cb", "size": 100, "vin": []map[string]any{{"coinbase": "03"}}, "vout": []map[string]any{{"value": 50.0}}}}}, nil
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    updateIndexes()
    mu.Lock()
    var got = calls
    mu.Unlock()
    if len(got) == 0 || got[0] != "getblockcount" {
        t.Fatalf("calls = %v, want the one tip fetch first", got)
    }
    var tips int
    for _, c := range got {
        if c == "getblockcount" { tips++ }
    }
    if tips != 1 {
        t.Errorf("getblockcount called %d times, want 1 for all five indexes", tips)
    }
    var lastCache, firstIndex = -1, -1
    for i, c := range got {
        if c == "getblock:2" { lastCache = i }
        if c == "getblock:3" && firstIndex == -1 { firstIndex = i }
    }
    if lastCache == -1 || firstIndex == -1 || lastCache > firstIndex {
        t.Errorf("calls = %v, want the block cache's getblocks before the address index's", got)
    }
    for h := 0; h <= 2; h++ {
        if _, ok := loadBlock(int64(h)); !ok {
            t.Errorf("block %d was not cached", h)
        }
    }
    if last, ok := cursors.Get(cursors.AddrIndex); !ok || last != 2 {
        t.Errorf("address index cursor = (%d, %v), want (2, true)", last, ok)
    }
    if _, ok := addrbal.Count(); !ok {
        t.Error("addrbal did not catch up")
    }
}
