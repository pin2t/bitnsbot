package addrindex

import "encoding/hex"
import "testing"

// Key and Decode have to agree for every form, since a scan matches a script
// against a set of addresses by meeting on the same key. Building the script and
// the address from the same payload is what pins that: the two arrive from
// opposite directions.
func TestKeyAndDecodeAgree(t *testing.T) {
    var h20 = mustHex(t, "62e907b15cbf27d5425399ebf6f0fb50ebb88f18")
    var h32 = mustHex(t, "701a8d401c84fb13e6baf169d59684e17abd9fa216c8cc5b9fc63d622ff8c58d")
    var pubkey = mustHex(t, "0378d430274f8c5ec1321338151e9f27f4c676a008bdf8638d07c0b6be9ab35c71")
    var p2pk = append(append([]byte{33}, pubkey...), 0xac)
    var cases = []struct {
        name   string
        script []byte
        kind   string
    }{
        {"p2pkh", append(append([]byte{0x76, 0xa9, 20}, h20...), 0x88, 0xac), "p2pkh"},
        {"p2sh", append(append([]byte{0xa9, 20}, h20...), 0x87), "p2sh"},
        {"segwit v0 key", append([]byte{0x00, 20}, h20...), "segwit"},
        {"segwit v0 script", append([]byte{0x00, 32}, h32...), "segwit"},
        {"taproot", append([]byte{0x51, 32}, h32...), "taproot"},
        // a P2PK output reads as the P2PKH address of its key, so the two forms
        // must land on one key or its payments would be missed
        {"p2pk", p2pk, "p2pkh"},
    }
    for _, c := range cases {
        var addr = Address(c.script)
        if addr == "" { t.Fatalf("%s: Address gave nothing", c.name) }
        var fromScript, ok = Key(c.script)
        if !ok { t.Fatalf("%s: Key(%x) gave nothing", c.name, c.script) }
        var fromAddr, kind, dok = Decode(addr)
        if !dok { t.Fatalf("%s: Decode(%s) gave nothing", c.name, addr) }
        if fromScript != fromAddr {
            t.Errorf("%s: script key %x != address key %x", c.name, fromScript, fromAddr)
        }
        if kind != c.kind { t.Errorf("%s: kind = %q, want %q", c.name, kind, c.kind) }
    }
}

// Real mainnet addresses, decoded to the payload their scripts carry.
func TestDecodeRealAddresses(t *testing.T) {
    var cases = map[string]struct {
        payload string
        kind    string
    }{
        // the genesis coinbase address
        "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa": {"62e907b15cbf27d5425399ebf6f0fb50ebb88f18", "p2pkh"},
        // the largest balance on the chain, a P2SH
        "34xp4vRoCGJym3xR7yCVPFHoCNxv4Twseo": {"23e522dfc6656a8fda3d47b4fa53f7585ac758cd", "p2sh"},
        // BIP-350's own bech32m vector
        "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0": {"79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798", "taproot"},
    }
    for addr, want := range cases {
        var key, kind, ok = Decode(addr)
        if !ok { t.Fatalf("Decode(%s) failed", addr) }
        if got := hex.EncodeToString([]byte(key[1:])); got != want.payload {
            t.Errorf("%s payload = %s, want %s", addr, got, want.payload)
        }
        if kind != want.kind { t.Errorf("%s kind = %q, want %q", addr, kind, want.kind) }
    }
}

// Anything that is not an address must be refused rather than decoded to some
// payload — the set a scan matches against is built from these.
func TestDecodeRejects(t *testing.T) {
    for _, s := range []string{
        "",
        "not an address",
        "1A1zP1eP5QGefi2DMPTfTL5SLmv7Divfna",                            // bad checksum
        "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t5",                    // bech32 checksum from the BIP's invalid list
        "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7v8n0nx0",  // BIP-350: a v1 program checksummed as bech32, not bech32m
        "3333333333333333333333333333333333",
    } {
        if _, _, ok := Decode(s); ok { t.Errorf("Decode(%q) was accepted", s) }
    }
}

// A script paying nobody — an OP_RETURN, a bare multisig — has no key, which is
// what keeps it out of the scan's map lookups entirely.
func TestKeyRejectsNonStandard(t *testing.T) {
    for _, s := range []string{"6a0401020304", "51210278d430274f8c5ec1321338151e9f27f4c676a008bdf8638d07c0b6be9ab35c7151ae", ""} {
        if _, ok := Key(mustHex(t, s)); ok { t.Errorf("Key(%s) was accepted", s) }
    }
}

func mustHex(t *testing.T, s string) []byte {
    t.Helper()
    var b, err = hex.DecodeString(s)
    if err != nil { t.Fatalf("hex %s: %v", s, err) }
    return b
}
