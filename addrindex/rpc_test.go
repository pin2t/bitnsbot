package addrindex

import "context"
import "encoding/json"
import "fmt"
import "os"
import "path/filepath"
import "reflect"
import "testing"
import "bitnsbot/core/coretest"

// Two real blocks from a Bitcoin Core v31.1.0 regtest node, as getblock
// verbosity 3 reported them (testdata/block<h>.json). Block 113's second
// transaction spends a P2PKH, a P2SH, a P2WPKH and a taproot output at once, and
// pays an OP_RETURN among its outputs; genesis is the block whose coinbase pays
// a bare public key.
//
// What the JSON is read into is checked against sources that share nothing with
// it: the outputs and the header's time against what /rest/block served for the
// same blocks, and the spent prevouts against what /rest/spenttxouts served for
// block 113. tools/addrindex holds its block-file parser to those same outputs,
// reading the serialized blocks themselves.
func TestRPCReadsTheBlock(t *testing.T) {
    var outputs = map[int][][]Payment{
        0: {{{Script: mustHex(t, "4104678afdb0fe5548271967f1a67130b7105cd6a828e03909a67962e0ea1f61deb649f6bc3f4cef38c4f35504e51ec112de5c384df7ba0b8d578a4c702b6bf11d5fac"), Sat: 5000000000}}},
        113: {{
            {Script: mustHex(t, "a914009a5f4eca9506b9fe82ae824a2d2187d9fc96a187"), Sat: 5000009300},
            {Script: mustHex(t, "6a24aa21a9ed1e1342cf2e2a885b117c9ec3bde85adb93c7a1276553ab5fb2bd59ee52880b3e"), Sat: 0},
        }, {
            {Script: mustHex(t, "76a914203b872d7db07e5dd6eac63603bad4432c57907688ac"), Sat: 50000000},
            {Script: mustHex(t, "51200738980554d28706a692599b8a6acb74a98bbb71c497dd0c0836194a82d923e0"), Sat: 1314990700},
            {Script: mustHex(t, "6a03626974"), Sat: 0},
        }},
    }
    var times = map[int]int64{0: 1296688602, 113: 1789369304}
    var spent113 = [][]Payment{{}, {
        {Script: mustHex(t, "76a9141a70d5f0332bf1cb792ed5786c510180283bf6f688ac"), Sat: 150000000},
        {Script: mustHex(t, "a914adb959cea47dbab9e9986f08b816fa5f0c41843487"), Sat: 225000000},
        {Script: mustHex(t, "001479f17b60a5a1826c50a49d6ebd0bb55b99afc637"), Sat: 680000000},
        {Script: mustHex(t, "5120e6e2debb61c212ca7f3efdd85be34fae01c922172718f6a573687d295cb3bcc0"), Sat: 310000000},
    }}
    for height, wantSpent := range map[int][][]Payment{0: {{}}, 113: spent113} {
        var blk, err = blockFrom(t, string(fixture(t, fmt.Sprintf("block%d.json", height))), height)
        if err != nil { t.Fatalf("block %d: %v", height, err) }
        if blk.Time != times[height] {
            t.Errorf("block %d: time %d, want the header's %d", height, blk.Time, times[height])
        }
        var wantOutputs = outputs[height]
        if len(blk.Txs) != len(wantOutputs) {
            t.Fatalf("block %d: %d transactions, want %d", height, len(blk.Txs), len(wantOutputs))
        }
        for i, tx := range blk.Txs {
            if !reflect.DeepEqual(tx.Outputs, wantOutputs[i]) {
                t.Errorf("block %d tx %d: outputs %v, want %v", height, i, tx.Outputs, wantOutputs[i])
            }
            if (len(tx.Spent) > 0 || len(wantSpent[i]) > 0) && !reflect.DeepEqual(tx.Spent, wantSpent[i]) {
                t.Errorf("block %d tx %d: spent %v, want %v", height, i, tx.Spent, wantSpent[i])
            }
        }
    }
}

// Core reports an amount as a BTC number, and a float's nearest value to one is
// often a hair under it: 0.29 BTC times 1e8 is 28999999.999999996. So the
// satoshi are rounded, never truncated — one short, and a balance summed from
// genesis ends a satoshi out for every such amount.
func TestRPCAmountsAreExactSatoshi(t *testing.T) {
    var blk, err = blockFrom(t, `{"tx":[{"vin":[`+
        `{"prevout":{"value":0.29,"scriptPubKey":{"hex":"51"}}},`+
        `{"prevout":{"value":20999999.97690000,"scriptPubKey":{"hex":"52"}}}],`+
        `"vout":[{"value":0.29,"scriptPubKey":{"hex":"53"}}]}]}`, 1)
    if err != nil { t.Fatalf("BlockAt: %v", err) }
    var tx = blk.Txs[0]
    if len(tx.Spent) != 2 || tx.Spent[0].Sat != 29000000 || tx.Spent[1].Sat != 2099999997690000 {
        t.Errorf("spent = %v, want 29000000 and 2099999997690000 sat", tx.Spent)
    }
    if len(tx.Outputs) != 1 || tx.Outputs[0].Sat != 29000000 {
        t.Errorf("outputs = %v, want 29000000 sat", tx.Outputs)
    }
}

// A script that is not hex is an error, not an empty script: an empty one is
// skipped by every scan, so the payment would vanish without a word.
func TestRPCRefusesAMalformedScript(t *testing.T) {
    var _, err = blockFrom(t, `{"tx":[{"vout":[{"value":1,"scriptPubKey":{"hex":"not hex"}}]}]}`, 1)
    if err == nil { t.Error("a script that is not hex was accepted") }
}

// blockFrom reads height through BlockAt from a node that knows one block,
// whatever height is asked for, and answers getblock at verbosity 3 with the JSON
// verbose.
func blockFrom(t *testing.T, verbose string, height int) (Block, error) {
    coretest.Start(t, func(method string, params []interface{}) (interface{}, error) {
        switch {
        case method == "getblockhash":
            return "fixture", nil
        case method == "getblock" && params[0] == "fixture" && params[1] == float64(3):
            return json.RawMessage(verbose), nil
        }
        return nil, fmt.Errorf("unexpected call %s %v", method, params)
    })
    return BlockAt(context.Background(), height)
}

func fixture(t *testing.T, name string) []byte {
    t.Helper()
    var b, err = os.ReadFile(filepath.Join("testdata", name))
    if err != nil { t.Fatalf("fixture: %v", err) }
    return b
}
