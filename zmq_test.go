package main

import "encoding/hex"
import "reflect"
import "testing"

// Real transactions captured from a Bitcoin Core regtest node, with the inputs
// and output scripts Core itself reported for them — a segwit spend, a
// three-output send, and a coinbase (whose input is the null outpoint and whose
// second output is an OP_RETURN witness commitment). Parsing has to agree with
// the node exactly, because a mismatch here means silently missing or inventing
// watch notifications.
func TestParseTx(t *testing.T) {
    var cases = []struct {
        name    string
        raw     string
        inputs  []outpoint
        outputs []string
    }{
        {
            name: "8ecde571dbd8",
            raw:  "020000000001011f9abe49ce4af64ac634ee3b151a2f2ac86533d7b8d007d2b8101867b4d647bd0000000000fdffffff02bcd6e40000000000160014f31640a577726488ea712f4b570e758bbe0cea508096980000000000160014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d802473044022033ee7f6288732a9505c2782fdacdbb20288cfbf2ccc63e26f95fd5fa2640445c02202e1e1cf91fafbb36a8b82d675b1bf90b7780b79b74dc2744884a75b40d0b191d012102be5d9890238a0cdbfdd3f421df030908f78b09b93f911f6c20d01058ecc478c167000000",
            inputs:  []outpoint{{"bd47d6b4671810b8d207d0b8d73365c82a2f1a153bee34c64af64ace49be9a1f", 0}},
            outputs: []string{"0014f31640a577726488ea712f4b570e758bbe0cea50", "0014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d8"},
        },
        {
            name: "01d2a3f7f8e3",
            raw:  "02000000000101a7ba6e78ddb6a979b4696ebeeb0c978094a627517ea4455f89b682867c7906270100000000fdffffff030cd8fa02000000001600146c365b76d169121a1220b2e472f4c4f4178d3036002d310100000000160014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d880c3c90100000000160014a95b146a45ea18e4d17d6ef35d99ea8217ff54d80247304402203c60b7d839f8688a4871d040ff93f885bacee49ff39829e9da60f7cfa7635fed02200f179c6198ae5a7ab4d94e51e9e0ddd8dbf73294e2de1cb60db9a58d7a67eaae012103fd4effc1cc3f2da8587fa1c1d36f5f24fb326b3673888caf1c68335a614b07b767000000",
            inputs:  []outpoint{{"2706797c8682b6895f45a47e5127a69480970cebbe6e69b479a9b6dd786ebaa7", 1}},
            outputs: []string{"00146c365b76d169121a1220b2e472f4c4f4178d3036", "0014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d8", "0014a95b146a45ea18e4d17d6ef35d99ea8217ff54d8"},
        },
        {
            name: "faee5caba920",
            raw:  "020000000001010000000000000000000000000000000000000000000000000000000000000000ffffffff03016800feffffff02740a062a01000000160014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d80000000000000000266a24aa21a9ede887852b50c23289795117c3ab5de19d8ec063f544be15b2dff04001cbf312000120000000000000000000000000000000000000000000000000000000000000000067000000",
            inputs:  []outpoint{{"0000000000000000000000000000000000000000000000000000000000000000", 4294967295}},
            outputs: []string{"0014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d8", "6a24aa21a9ede887852b50c23289795117c3ab5de19d8ec063f544be15b2dff04001cbf31200"},
        },
    }
    for _, c := range cases {
        var raw, err = hex.DecodeString(c.raw)
        if err != nil { t.Fatalf("%s: bad fixture: %v", c.name, err) }
        var tx, ok = parseTx(raw)
        if !ok { t.Fatalf("%s: parseTx failed", c.name) }
        if !reflect.DeepEqual(tx.inputs, c.inputs) {
            t.Errorf("%s: inputs = %v, want %v", c.name, tx.inputs, c.inputs)
        }
        if !reflect.DeepEqual(tx.outputScripts, c.outputs) {
            t.Errorf("%s: output scripts = %v, want %v", c.name, tx.outputScripts, c.outputs)
        }
    }
}

// Truncated or malformed input must be rejected rather than panic — these bytes
// come straight off the wire. Note the contract this pins: parseTx stops at the
// end of the last output and never reads the witness or locktime, so truncating
// *those* trailing bytes is legitimately not an error. Only cuts landing inside
// the version, inputs or outputs are.
func TestParseTxRejectsGarbage(t *testing.T) {
    var full, err = hex.DecodeString("020000000001011f9abe49ce4af64ac634ee3b151a2f2ac86533d7b8d007d2b8101867b4d647bd0000000000fdffffff02bcd6e40000000000160014f31640a577726488ea712f4b570e758bbe0cea508096980000000000160014e1b17a7a9c2d40669fc4f43b21fabd8dd8ba13d802473044022033ee7f6288732a9505c2782fdacdbb20288cfbf2ccc63e26f95fd5fa2640445c02202e1e1cf91fafbb36a8b82d675b1bf90b7780b79b74dc2744884a75b40d0b191d012102be5d9890238a0cdbfdd3f421df030908f78b09b93f911f6c20d01058ecc478c167000000")
    if err != nil { t.Fatalf("bad fixture: %v", err) }
    // version 4 + marker/flag 2 + one 41-byte input + count 1 + two 31-byte outputs
    var outputsEnd = 4 + 2 + 1 + 41 + 1 + 31 + 31
    for n := 0; n < outputsEnd; n++ {
        if _, ok := parseTx(full[:n]); ok {
            t.Fatalf("parseTx accepted a %d-byte prefix, which cuts into the outputs", n)
        }
    }
    if _, ok := parseTx(full[:outputsEnd]); !ok {
        t.Fatalf("parseTx rejected a transaction truncated to exactly its outputs (%d bytes)", outputsEnd)
    }
    for _, garbage := range [][]byte{{}, {0xff, 0xff, 0xff}, {0x02, 0x00, 0x00, 0x00, 0xfd}} {
        if _, ok := parseTx(garbage); ok {
            t.Errorf("parseTx accepted garbage %x", garbage)
        }
    }
}
