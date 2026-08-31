package main

import "encoding/hex"
import "strings"
import "testing"

// The published RIPEMD-160 vectors. Nothing else in this repo hashes with it, so
// a mistake here would surface only as wrong addresses in the database.
func TestRipemd160Vectors(t *testing.T) {
    for _, c := range []struct{ in, want string }{
        {"", "9c1185a5c5e9fc54612808977ee8f548b2258d31"},
        {"a", "0bdc9d2d256b3ee9daae347be6f4dc835a467ffe"},
        {"abc", "8eb208f7e05d987a9b044a8e98c6b087f15a0bfc"},
        {"message digest", "5d0689ef49d2fae572b881b123a85ffa21595f36"},
        {"abcdefghijklmnopqrstuvwxyz", "f71c27109c692c1b56bbdceb5b9d2865b3708dbc"},
        {strings.Repeat("1234567890", 8), "9b752e45573d4b39f4dbd3323cab82bf63326bfb"},
    } {
        if got := hex.EncodeToString(ripemd160([]byte(c.in))); got != c.want {
            t.Errorf("ripemd160(%q) = %s, want %s", c.in, got, c.want)
        }
    }
    // a message that spans two blocks, where the padding lands in the second
    if got := hex.EncodeToString(ripemd160([]byte(strings.Repeat("a", 1000000)))); got != "52783243c1697bdbe16d37f97f68f08325dc1528" {
        t.Errorf("a million a's = %s", got)
    }
}

// Real, well-known addresses: each script is what the chain actually carries and
// each address is what the world knows it as.
func TestScriptAddress(t *testing.T) {
    for _, c := range []struct{ name, script, want string }{
        // the genesis coinbase, a bare public key — Core reports the P2PKH form
        {"P2PK", "4104678afdb0fe5548271967f1a67130b7105cd6a828e03909a67962e0ea1f61deb649f6bc3f4cef38c4f35504e51ec112de5c384df7ba0b8d578a4c702b6bf11d5fac",
            "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"},
        // Satoshi's other well-known address, as a P2PKH script
        {"P2PKH", "76a91462e907b15cbf27d5425399ebf6f0fb50ebb88f1888ac",
            "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"},
        // BIP-173's own examples
        {"P2WPKH", "0014751e76e8199196d454941c45d1b3a323f1433bd6",
            "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
        {"P2WSH", "00201863143c14c5166804bd19203356da136c985678cd4d27a1b8c6329604903262",
            "bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3"},
        // BIP-350's taproot example, which is bech32m rather than bech32
        {"P2TR", "512079be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
            "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0"},
    } {
        var script, err = hex.DecodeString(c.script)
        if err != nil { t.Fatalf("%s: %v", c.name, err) }
        if got := scriptAddress(script); got != c.want {
            t.Errorf("%s = %q, want %q", c.name, got, c.want)
        }
    }
}

// P2SH has its own version byte, so it must not come out as a P2PKH address.
func TestScriptAddressP2SH(t *testing.T) {
    var script, _ = hex.DecodeString("a91474f209f6ea907e2ea48f74fae05782ae8a66525787")
    var got = scriptAddress(script)
    if !strings.HasPrefix(got, "3") {
        t.Errorf("P2SH = %q, want an address starting with 3", got)
    }
    // cross-checked against an independent base58check implementation
    if got != "3CMNFxN1oHBc4R1EpboAL5yzHGgE611Xou" {
        t.Errorf("P2SH = %q", got)
    }
}

// Anything that pays to no address must say so rather than inventing one.
func TestScriptAddressNonStandard(t *testing.T) {
    for _, c := range []struct{ name, script string }{
        {"OP_RETURN", "6a0b68656c6c6f20776f726c64"},
        {"bare multisig", "5121026477115981fe981a6918a6297d9803c4dc04f328f22041bedff886bbc2962e0121038a" +
            "b45770d5c8e6b1e0c1a1b1e1c1d1e1f1a1b1c1d1e1f1a1b1c1d1e1f1a1b1c1d52ae"},
        {"empty", ""},
        {"truncated P2PKH", "76a914000102"},
        {"witness with a bad length", "0003010203"},
    } {
        var script, _ = hex.DecodeString(c.script)
        if got := scriptAddress(script); got != "" {
            t.Errorf("%s = %q, want no address", c.name, got)
        }
    }
}

// Witness versions past 0 use bech32m, and a v0 program is only ever 20 or 32
// bytes — the two rules BIP-350 turns on.
func TestWitnessRules(t *testing.T) {
    // v0 with a 22-byte program is not a valid v0 output
    var bad, _ = hex.DecodeString("0016" + strings.Repeat("00", 22))
    if got := scriptAddress(bad); got != "" {
        t.Errorf("a 22-byte v0 program produced %q", got)
    }
    // v1 with 32 bytes is taproot and encodes with the bech32m constant, which
    // makes it differ from the same program read as v0
    var v1, _ = hex.DecodeString("5120" + strings.Repeat("11", 32))
    var v0, _ = hex.DecodeString("0020" + strings.Repeat("11", 32))
    var a, b = scriptAddress(v1), scriptAddress(v0)
    if a == "" || b == "" { t.Fatal("both should encode") }
    if !strings.HasPrefix(a, "bc1p") { t.Errorf("v1 = %q, want a bc1p address", a) }
    if !strings.HasPrefix(b, "bc1q") { t.Errorf("v0 = %q, want a bc1q address", b) }
}
