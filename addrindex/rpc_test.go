package addrindex

import "context"
import "encoding/binary"
import "encoding/hex"
import "encoding/json"
import "fmt"
import "os"
import "path/filepath"
import "reflect"
import "testing"

// Two real blocks from a Bitcoin Core v31.1.0 regtest node, each captured twice:
// as getblock verbosity 3 reported it (testdata/block<h>.json), and as the REST
// interface served the same block serialized (block<h>.hex). Block 113's second
// transaction spends a P2PKH, a P2SH, a P2WPKH and a taproot output at once, and
// pays an OP_RETURN among its outputs; genesis is the block whose coinbase pays
// a bare public key.
//
// What the JSON is read into is checked against sources that share nothing with
// it: the outputs and the time against the serialized block, through the parser
// actbuild reads block files with, and the spent prevouts against what
// /rest/spenttxouts served for block 113.
func TestRPCReadsTheBlock(t *testing.T) {
    var spent113 = [][]Payment{{}, {
        {Script: mustHex(t, "76a9141a70d5f0332bf1cb792ed5786c510180283bf6f688ac"), Sat: 150000000},
        {Script: mustHex(t, "a914adb959cea47dbab9e9986f08b816fa5f0c41843487"), Sat: 225000000},
        {Script: mustHex(t, "001479f17b60a5a1826c50a49d6ebd0bb55b99afc637"), Sat: 680000000},
        {Script: mustHex(t, "5120e6e2debb61c212ca7f3efdd85be34fae01c922172718f6a573687d295cb3bcc0"), Sat: 310000000},
    }}
    for height, wantSpent := range map[int][][]Payment{0: {{}}, 113: spent113} {
        var blk, err = NewRPC(reply(string(fixture(t, fmt.Sprintf("block%d.json", height))))).BlockAt(context.Background(), height)
        if err != nil { t.Fatalf("block %d: %v", height, err) }
        var raw, _ = hex.DecodeString(string(fixture(t, fmt.Sprintf("block%d.hex", height))))
        var wantOutputs, ok = parseBlockOutputs(raw)
        if !ok { t.Fatalf("block %d: the serialized fixture does not parse", height) }
        if want := int64(binary.LittleEndian.Uint32(raw[68:72])); blk.Time != want {
            t.Errorf("block %d: time %d, want the header's %d", height, blk.Time, want)
        }
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
    var blk, err = NewRPC(reply(`{"tx":[{"vin":[`+
        `{"prevout":{"value":0.29,"scriptPubKey":{"hex":"51"}}},`+
        `{"prevout":{"value":20999999.97690000,"scriptPubKey":{"hex":"52"}}}],`+
        `"vout":[{"value":0.29,"scriptPubKey":{"hex":"53"}}]}]}`)).BlockAt(context.Background(), 1)
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
    var _, err = NewRPC(reply(`{"tx":[{"vout":[{"value":1,"scriptPubKey":{"hex":"not hex"}}]}]}`)).BlockAt(context.Background(), 1)
    if err == nil { t.Error("a script that is not hex was accepted") }
}

// reply is a node that knows one block, whatever height is asked for.
func reply(verbose string) Caller {
    return func(ctx context.Context, method string, params []interface{}, result interface{}) error {
        switch {
        case method == "getblockhash":
            *result.(*string) = "fixture"
            return nil
        case method == "getblock" && params[0] == "fixture" && params[1] == 3:
            return json.Unmarshal([]byte(verbose), result)
        }
        return fmt.Errorf("unexpected call %s %v", method, params)
    }
}

func fixture(t *testing.T, name string) []byte {
    t.Helper()
    var b, err = os.ReadFile(filepath.Join("testdata", name))
    if err != nil { t.Fatalf("fixture: %v", err) }
    return b
}
