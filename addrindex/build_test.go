package addrindex

import "encoding/hex"
import "reflect"
import "testing"

// Real blocks captured from a Bitcoin Core v31.1.0 regtest node via its REST
// interface (/rest/block/<hash>.bin and /rest/spenttxouts/<hash>.bin), with the
// per-transaction output and spent-prevout scripts Core itself reported via
// getblock verbosity 3 for the same blocks. block102 has one coinbase and one
// spend; block103 has a coinbase and two further spends chained off it. Parsing
// has to agree with the node exactly, since a mismatch means silently missing or
// fabricating address history.
type blockCase struct {
    name        string
    block       string
    spent       string
    wantOutputs [][]string
    wantSpent   [][]string
}

func parseBlockCases() []blockCase {
    return []blockCase{
        {
            name:  "block102",
            block: "000000202b687778eddbeeb89f1551e63aa7505b461f396f09e3b92f23ba83aa221c243a416b1476753995b3ce8b34a9de6729c37f50b1c4576e53e1fb9eb7f5c25bceb6e7205e6affff7f200000000002020000000001010000000000000000000000000000000000000000000000000000000000000000ffffffff03016600feffffff0204fd052a01000000160014d2b2f31918bdd57ca878317b5c639ec0a739e2690000000000000000266a24aa21a9ed27790064491a6e6e5ebc94766e0e173b5c62be6ab9c3cd133c32b2c7284f4137012000000000000000000000000000000000000000000000000000000000000000006500000002000000000101c08f084fd33d0a5207255c89e0340417dc4fe0cb8acbf26b345a202667abdc630000000000fdffffff027c15152101000000160014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec80d1f00800000000160014a6d49256e4f923822286832a77d7e2a909a4c7620247304402204a75bca110a1800c02566bf94f2ef7705902ea1df66280361d9a61543e4a2ec1022007351332e5630462fe83972dafb74a4614a43bc613834a707daed17221ff6612012102cccf1e47f3a6326ed4101c7185542a2844fd25076390555dfc04705ecf48468f65000000",
            spent: "02000100f2052a01000000160014d2b2f31918bdd57ca878317b5c639ec0a739e269",
            wantOutputs: [][]string{{"0014d2b2f31918bdd57ca878317b5c639ec0a739e269", "6a24aa21a9ed27790064491a6e6e5ebc94766e0e173b5c62be6ab9c3cd133c32b2c7284f4137"}, {"0014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec", "0014a6d49256e4f923822286832a77d7e2a909a4c762"}},
            wantSpent:   [][]string{{}, {"0014d2b2f31918bdd57ca878317b5c639ec0a739e269"}},
        },
        {
            name:  "block103",
            block: "000000209adb0270161c90be20851202870014d8d27d98e1832ed960dea0392a1ae7292d13e5e72d08a2dbd9e678ece47c21644f037796143766b3161c15b3ee79ae9aca51215e6affff7f200000000003020000000001010000000000000000000000000000000000000000000000000000000000000000ffffffff03016700feffffff020808062a01000000160014d2b2f31918bdd57ca878317b5c639ec0a739e2690000000000000000266a24aa21a9ed00b1a6de804a7485ba221e46946f767afacc26540da46079027797aad8f31c8f012000000000000000000000000000000000000000000000000000000000000000006600000002000000000101e538de5326756d73685cfac914d6675b149ede18b5f4dfb06f37f90773cde8f40000000000fdffffff0240787d010000000016001467efcb63b3a1135979ecc3a00956ef07dea371cc3892971f01000000160014168a5d01b286b82426aa34edd402322c1d7b3cd602473044022015a670c5430ba3b2c770f0471bd6d3d30185d65fe32bfa239f895aff3f0882f20220354a61aeeadb75f694269647ca75efb63b8826ad3e50e831b90bd4e25ccdec72012103c0c512d5d970876c6ec681c298930265d899985cef0b58f230ef0d74acbfd3d26600000002000000000101e538de5326756d73685cfac914d6675b149ede18b5f4dfb06f37f90773cde8f40100000000fdffffff0280f0fa0200000000160014705e4a46bed92fff46ad9a1eab968a637e8055b8fcd5f505000000001600142cdf30a44b7eb15da3346626e3ae5c9c3aa09607024730440220723dd8e9aec6f5a2f8acc7476a1864de5b99b86a27a2f245c26368538e1798630220271c7b873e5c56ada876832b620c36186629d53f67312ca4e41e0eac4deb89c501210360a37afc68b928733d6891eb33f3865799b7cd2699583175e6d9fc9c3e9f670f66000000",
            spent: "0300017c15152101000000160014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec0180d1f00800000000160014a6d49256e4f923822286832a77d7e2a909a4c762",
            wantOutputs: [][]string{{"0014d2b2f31918bdd57ca878317b5c639ec0a739e269", "6a24aa21a9ed00b1a6de804a7485ba221e46946f767afacc26540da46079027797aad8f31c8f"}, {"001467efcb63b3a1135979ecc3a00956ef07dea371cc", "0014168a5d01b286b82426aa34edd402322c1d7b3cd6"}, {"0014705e4a46bed92fff46ad9a1eab968a637e8055b8", "00142cdf30a44b7eb15da3346626e3ae5c9c3aa09607"}},
            wantSpent:   [][]string{{}, {"0014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec"}, {"0014a6d49256e4f923822286832a77d7e2a909a4c762"}},
        },
    }
}

func TestParseBlock(t *testing.T) {
    for _, c := range parseBlockCases() {
        var blockBytes, err = hex.DecodeString(c.block)
        if err != nil { t.Fatalf("%s: bad block fixture: %v", c.name, err) }
        var spentBytes, serr = hex.DecodeString(c.spent)
        if serr != nil { t.Fatalf("%s: bad spent fixture: %v", c.name, serr) }

        var outputs, ok = parseBlockOutputs(blockBytes)
        if !ok { t.Fatalf("%s: parseBlockOutputs failed", c.name) }
        var gotOutputs = hexLists(outputs)
        if !reflect.DeepEqual(gotOutputs, c.wantOutputs) {
            t.Errorf("%s: outputs = %v, want %v", c.name, gotOutputs, c.wantOutputs)
        }

        var spent, sok = parseSpentOutputs(spentBytes)
        if !sok { t.Fatalf("%s: parseSpentOutputs failed", c.name) }
        var gotSpent = hexLists(spent)
        if !reflect.DeepEqual(gotSpent, c.wantSpent) {
            t.Errorf("%s: spent = %v, want %v", c.name, gotSpent, c.wantSpent)
        }
    }
}

func hexLists(scripts [][][]byte) [][]string {
    var out = make([][]string, len(scripts))
    for i, list := range scripts {
        out[i] = make([]string, len(list))
        for j, s := range list {
            out[i][j] = hex.EncodeToString(s)
        }
    }
    return out
}

// indexBlock must dedupe a transaction touching the same address more than once
// — the real regtest fixture already has block103's tx0 doing exactly that: its
// coinbase pays the same watched-style address twice via a spend/return pattern
// is not present here, so this constructs the case directly against the parsed
// output/spent shapes rather than needing another mined fixture.
func TestIndexBlockDedups(t *testing.T) {
    var outputs = [][][]byte{{[]byte("scriptA"), []byte("scriptA"), []byte("scriptB")}}
    var spent = [][][]byte{{[]byte("scriptA")}} // same tx also spends from scriptA
    var touches = map[string][]Touch{}
    indexBlockFromParsed(touches, 42, outputs, spent)
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
func TestIndexBlockRealChain(t *testing.T) {
    var b102 = testBlock(t, "block102")
    var b103 = testBlock(t, "block103")
    var touches = map[string][]Touch{}
    indexBlock(touches, 102, b102)
    indexBlock(touches, 103, b103)
    // 0014ca66... is created by block102/tx1 and spent by block103/tx1
    var script, _ = hex.DecodeString("0014ca66b88f1306d41e7e74a2eda7c1ad95e8060eec")
    var want = []Touch{{Height: 102, TxIndex: 1}, {Height: 103, TxIndex: 1}}
    if got := touches[string(Prefix(script))]; !reflect.DeepEqual(got, want) {
        t.Fatalf("touches = %v, want %v", got, want)
    }
}

func testBlock(t *testing.T, name string) Block {
    for _, c := range parseBlockCases() {
        if c.name == name {
            var raw, _ = hex.DecodeString(c.block)
            var spent, _ = hex.DecodeString(c.spent)
            return Block{Hash: name, Raw: raw, Spent: spent}
        }
    }
    t.Fatalf("no fixture named %s", name)
    return Block{}
}
