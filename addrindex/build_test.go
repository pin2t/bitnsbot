package addrindex

import "encoding/hex"
import "reflect"
import "testing"

// Real blocks from a Bitcoin Core v31.1.0 regtest node, as the per-transaction
// output and spent-prevout scripts Core itself reported via getblock verbosity 3.
// block102 has one coinbase and one spend; block103 has a coinbase and two
// further spends chained off it.
type blockCase struct {
    name    string
    outputs [][]string
    spent   [][]string
}

func blockCases() []blockCase {
    return []blockCase{
        {
            name:    "block102",
            outputs: [][]string{{"0014d2b2f31918bdd57ca878317b5c639ec0a739e269", "6a24aa21a9ed27790064491a6e6e5ebc94766e0e173b5c62be6ab9c3cd133c32b2c7284f4137"}, {"0014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec", "0014a6d49256e4f923822286832a77d7e2a909a4c762"}},
            spent:   [][]string{{}, {"0014d2b2f31918bdd57ca878317b5c639ec0a739e269"}},
        },
        {
            name:    "block103",
            outputs: [][]string{{"0014d2b2f31918bdd57ca878317b5c639ec0a739e269", "6a24aa21a9ed00b1a6de804a7485ba221e46946f767afacc26540da46079027797aad8f31c8f"}, {"001467efcb63b3a1135979ecc3a00956ef07dea371cc", "0014168a5d01b286b82426aa34edd402322c1d7b3cd6"}, {"0014705e4a46bed92fff46ad9a1eab968a637e8055b8", "00142cdf30a44b7eb15da3346626e3ae5c9c3aa09607"}},
            spent:   [][]string{{}, {"0014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec"}, {"0014a6d49256e4f923822286832a77d7e2a909a4c762"}},
        },
    }
}

// indexBlock must dedupe a transaction touching the same address more than once:
// two outputs to one script, and the same script spent from in the same
// transaction, are one touch.
func TestIndexBlockDedups(t *testing.T) {
    var blk = Block{Txs: []Tx{{
        Outputs: []Payment{{Script: []byte("scriptA")}, {Script: []byte("scriptA")}, {Script: []byte("scriptB")}},
        Spent:   []Payment{{Script: []byte("scriptA")}},
    }}}
    var touches = map[string][]Touch{}
    indexBlock(touches, 42, blk)
    var a = touches[string(Prefix([]byte("scriptA")))]
    var b = touches[string(Prefix([]byte("scriptB")))]
    if len(a) != 1 || a[0] != (Touch{Height: 42, TxIndex: 0}) {
        t.Fatalf("scriptA touches = %v, want exactly one (dedup across output+output+spent)", a)
    }
    if len(b) != 1 || b[0] != (Touch{Height: 42, TxIndex: 0}) {
        t.Fatalf("scriptB touches = %v, want exactly one", b)
    }
}

// Real regtest blocks 102 and 103 chained together: block 102's tx1 pays two
// fresh addresses, block 103 spends one of them (via tx1) and the other (via
// tx2) and pays four more. Indexing both blocks must produce the exact chain of
// touches a wallet's transaction history should show.
//
// 0014ca66... is created by block102/tx1 and spent by block103/tx1
func TestIndexBlockRealChain(t *testing.T) {
    var b102 = testBlock(t, "block102")
    var b103 = testBlock(t, "block103")
    var touches = map[string][]Touch{}
    indexBlock(touches, 102, b102)
    indexBlock(touches, 103, b103)
    var script, _ = hex.DecodeString("0014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec")
    var want = []Touch{{Height: 102, TxIndex: 1}, {Height: 103, TxIndex: 1}}
    if got := touches[string(Prefix(script))]; !reflect.DeepEqual(got, want) {
        t.Fatalf("touches = %v, want %v", got, want)
    }
}

// testBlock is a fixture as a Block, built from the scripts the node reported.
func testBlock(t *testing.T, name string) Block {
    var payments = func(scripts []string) []Payment {
        var out []Payment
        for _, script := range scripts { out = append(out, Payment{Script: mustHex(t, script)}) }
        return out
    }
    for _, c := range blockCases() {
        if c.name != name { continue }
        var blk = Block{Hash: name}
        for i := range c.outputs {
            blk.Txs = append(blk.Txs, Tx{Outputs: payments(c.outputs[i]), Spent: payments(c.spent[i])})
        }
        return blk
    }
    t.Fatalf("no fixture named %s", name)
    return Block{}
}
