package addrindex

import "bytes"
import "crypto/sha256"
import "strings"

// This file is the one place that knows how an address is written, the same way
// build.go is the one place that knows how a block is. It turns a scriptPubKey
// into the address it pays (Address), and an address back into the payload a
// script carries it as (Decode) — so a scan that reads Core's files needs no node
// to ask, which was the last thing tying tools/addrindex's passes to the RPC
// interface, and is what lets the bot's address statistics match a script against
// a set of addresses without encoding one.
//
// Everything here is fully specified and pinned against published vectors: the
// base58 alphabet and checksum, BIP-173's bech32 for witness v0, BIP-350's
// bech32m for v1 and later, and RIPEMD-160 for the one case that needs to hash a
// public key itself.

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// mainnet version bytes and the human-readable part its bech32 addresses carry.
const p2pkhVersion = 0x00
const p2shVersion = 0x05
const hrp = "bc"

// Address returns the address a scriptPubKey pays to, or "" when it pays
// to none — an OP_RETURN, a bare multisig, anything nonstandard. These are the
// same forms Core's decodescript reports an address for.
func Address(script []byte) string {
    switch {
    // P2PKH: OP_DUP OP_HASH160 <20> OP_EQUALVERIFY OP_CHECKSIG
    case len(script) == 25 && script[0] == 0x76 && script[1] == 0xa9 && script[2] == 20 &&
        script[23] == 0x88 && script[24] == 0xac:
        return base58Check(p2pkhVersion, script[3:23])
    // P2SH: OP_HASH160 <20> OP_EQUAL
    case len(script) == 23 && script[0] == 0xa9 && script[1] == 20 && script[22] == 0x87:
        return base58Check(p2shVersion, script[2:22])
    // P2PK: <pubkey> OP_CHECKSIG — no hash in the script, so the key is hashed
    case len(script) == 35 && script[0] == 33 && script[34] == 0xac:
        return base58Check(p2pkhVersion, Hash160(script[1:34]))
    case len(script) == 67 && script[0] == 65 && script[66] == 0xac:
        return base58Check(p2pkhVersion, Hash160(script[1:66]))
    }
    if version, program, ok := witness(script); ok {
        return segwit(version, program)
    }
    return ""
}

// witness recognises a witness program: a version opcode followed by a single
// push of 2 to 40 bytes, and nothing else.
func witness(script []byte) (version byte, program []byte, ok bool) {
    if len(script) < 4 || len(script) > 42 { return 0, nil, false }
    var op = script[0]
    switch {
    case op == 0x00:
        version = 0
    case op >= 0x51 && op <= 0x60: // OP_1 .. OP_16
        version = op - 0x50
    default:
        return 0, nil, false
    }
    var n = int(script[1])
    if n < 2 || n > 40 || len(script) != n+2 { return 0, nil, false }
    // v0 is only ever a 20-byte key hash or a 32-byte script hash
    if version == 0 && n != 20 && n != 32 { return 0, nil, false }
    return version, script[2:], true
}

// base58Check encodes a version byte and payload the way a legacy address is
// written: the four-byte double-SHA-256 checksum appended, then base58, with one
// leading '1' per leading zero byte.
func base58Check(version byte, payload []byte) string {
    var full = make([]byte, 0, 1+len(payload)+4)
    full = append(full, version)
    full = append(full, payload...)
    var first = sha256.Sum256(full)
    var second = sha256.Sum256(first[:])
    full = append(full, second[:4]...)
    return base58(full)
}

func base58(b []byte) string {
    // long division of the whole number by 58, which is what the encoding is
    var digits = []byte{0}
    for _, c := range b {
        var carry = int(c)
        for i := range digits {
            carry += int(digits[i]) << 8
            digits[i] = byte(carry % 58)
            carry /= 58
        }
        for carry > 0 {
            digits = append(digits, byte(carry%58))
            carry /= 58
        }
    }
    var out = make([]byte, 0, len(digits)+len(b))
    // a leading zero byte is not a digit but a character in its own right
    for _, c := range b {
        if c != 0 { break }
        out = append(out, base58Alphabet[0])
    }
    for i := len(digits) - 1; i >= 0; i-- {
        out = append(out, base58Alphabet[digits[i]])
    }
    return string(out)
}

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// segwit encodes a witness program as bech32 (version 0) or bech32m (version 1
// and later), which is the split BIP-350 introduced after the original scheme
// was found to have an insertion weakness.
func segwit(version byte, program []byte) string {
    var data = append([]byte{version}, convertBits(program, 8, 5, true)...)
    var constant = 1
    if version > 0 { constant = 0x2bc830a3 }
    var out = hrp + "1"
    for _, d := range data { out += string(charset[d]) }
    for _, d := range checksum(data, constant) { out += string(charset[d]) }
    return out
}

// convertBits regroups a byte string into 5-bit groups, which is the alphabet
// bech32 spells addresses in.
func convertBits(data []byte, from, to uint, pad bool) []byte {
    var acc, bits uint
    var out []byte
    var max = byte(1<<to - 1)
    for _, b := range data {
        acc = acc<<from | uint(b)
        bits += from
        for bits >= to {
            bits -= to
            out = append(out, byte(acc>>bits)&max)
        }
    }
    if pad && bits > 0 { out = append(out, byte(acc<<(to-bits))&max) }
    return out
}

func checksum(data []byte, constant int) []byte {
    var values = append(expandHRP(), data...)
    values = append(values, 0, 0, 0, 0, 0, 0)
    var mod = polymod(values) ^ constant
    var out = make([]byte, 6)
    for i := range out { out[i] = byte(mod>>uint(5*(5-i))) & 31 }
    return out
}

func expandHRP() []byte {
    var out []byte
    for i := 0; i < len(hrp); i++ { out = append(out, hrp[i]>>5) }
    out = append(out, 0)
    for i := 0; i < len(hrp); i++ { out = append(out, hrp[i]&31) }
    return out
}

func polymod(values []byte) int {
    var gen = []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
    var chk = 1
    for _, v := range values {
        var top = chk >> 25
        chk = (chk&0x1ffffff)<<5 ^ int(v)
        for i := 0; i < 5; i++ {
            if top>>uint(i)&1 == 1 { chk ^= gen[i] }
        }
    }
    return chk
}

// Hash160 is RIPEMD-160 of SHA-256, which is how a public key becomes the 20
// bytes an address carries.
func Hash160(b []byte) []byte {
    var sum = sha256.Sum256(b)
    return ripemd160(sum[:])
}

// The lookup key a script and an address are matched by: one byte naming the
// form, then the payload the address carries — a 20-byte hash for the legacy
// forms, the witness program for the rest. Key reads it straight out of a
// script, Decode out of an address, and they agree by construction.
//
// Matching this way rather than by the address text is what makes scanning the
// chain for a set of addresses affordable. Encoding a script to its address
// measures 278 ns for P2PKH, 1.1 µs for segwit and 1.7 µs for taproot, and the
// chain has some six billion outputs and spent prevouts to put through it —
// hours of pure encoding. Key is a handful of byte comparisons and a slice.
const keyP2PKH = 0x00
const keyP2SH = 0x05

// witness forms are named by their version with the high bit set, which cannot
// collide with the two legacy version bytes above.
func keyWitness(version byte) byte { return 0x80 | version }

// Key is the lookup key for a scriptPubKey, or ok=false when the script pays to
// no address at all. P2PK is the one form that costs a hash — the script carries
// a public key rather than its hash — and it is confined to the early chain.
func Key(script []byte) (string, bool) {
    switch {
    case len(script) == 25 && script[0] == 0x76 && script[1] == 0xa9 && script[2] == 20 &&
        script[23] == 0x88 && script[24] == 0xac:
        return string(append([]byte{keyP2PKH}, script[3:23]...)), true
    case len(script) == 23 && script[0] == 0xa9 && script[1] == 20 && script[22] == 0x87:
        return string(append([]byte{keyP2SH}, script[2:22]...)), true
    case len(script) == 35 && script[0] == 33 && script[34] == 0xac:
        return string(append([]byte{keyP2PKH}, Hash160(script[1:34])...)), true
    case len(script) == 67 && script[0] == 65 && script[66] == 0xac:
        return string(append([]byte{keyP2PKH}, Hash160(script[1:66])...)), true
    }
    if version, program, ok := witness(script); ok {
        return string(append([]byte{keyWitness(version)}, program...)), true
    }
    return "", false
}

// Decode turns an address back into the key a script matching it would have, and
// names its form. It is Address's inverse for every form Address produces, which
// TestAddressRoundTrip pins: a P2PK script reads as the P2PKH address of its key,
// so the two meet on the same key and a P2PK output is counted for that address
// rather than missed.
func Decode(addr string) (key string, kind string, ok bool) {
    if strings.HasPrefix(addr, hrp+"1") || strings.HasPrefix(addr, strings.ToUpper(hrp)+"1") {
        var version, program, vok = decodeSegwit(strings.ToLower(addr))
        if !vok { return "", "", false }
        var name = "segwit"
        if version >= 1 { name = "taproot" }
        return string(append([]byte{keyWitness(version)}, program...)), name, true
    }
    var version, payload, bok = base58CheckDecode(addr)
    if !bok || len(payload) != 20 { return "", "", false }
    switch version {
    case p2pkhVersion:
        return string(append([]byte{keyP2PKH}, payload...)), "p2pkh", true
    case p2shVersion:
        return string(append([]byte{keyP2SH}, payload...)), "p2sh", true
    }
    return "", "", false
}

func base58CheckDecode(s string) (version byte, payload []byte, ok bool) {
    var full, dok = base58Decode(s)
    if !dok || len(full) < 5 { return 0, nil, false }
    var body, sum = full[:len(full)-4], full[len(full)-4:]
    var first = sha256.Sum256(body)
    var second = sha256.Sum256(first[:])
    if !bytes.Equal(sum, second[:4]) { return 0, nil, false }
    return body[0], body[1:], true
}

func base58Decode(s string) ([]byte, bool) {
    var num = []byte{0}
    for i := 0; i < len(s); i++ {
        var digit = strings.IndexByte(base58Alphabet, s[i])
        if digit < 0 { return nil, false }
        var carry = digit
        for j := range num {
            carry += int(num[j]) * 58
            num[j] = byte(carry)
            carry >>= 8
        }
        for carry > 0 {
            num = append(num, byte(carry))
            carry >>= 8
        }
    }
    var out []byte
    // a leading '1' is a zero byte, not a digit — the encoder's own convention
    for i := 0; i < len(s) && s[i] == base58Alphabet[0]; i++ { out = append(out, 0) }
    for i := len(num) - 1; i >= 0; i-- { out = append(out, num[i]) }
    return out, true
}

// decodeSegwit is segwit's inverse: it checks the checksum against the constant
// the version calls for — bech32 for v0, bech32m for v1 and later, the split
// BIP-350 introduced — so an address in the wrong scheme is rejected rather than
// silently accepted as another program.
func decodeSegwit(addr string) (version byte, program []byte, ok bool) {
    var sep = strings.LastIndexByte(addr, '1')
    if sep < 0 || addr[:sep] != hrp || len(addr) < sep+8 { return 0, nil, false }
    var data []byte
    for i := sep + 1; i < len(addr); i++ {
        var d = strings.IndexByte(charset, addr[i])
        if d < 0 { return 0, nil, false }
        data = append(data, byte(d))
    }
    version = data[0]
    if version > 16 { return 0, nil, false }
    var constant = 1
    if version > 0 { constant = 0x2bc830a3 }
    var values = append(expandHRP(), data...)
    if polymod(values) != constant { return 0, nil, false }
    program = convertBits(data[1:len(data)-6], 5, 8, false)
    if len(program) < 2 || len(program) > 40 { return 0, nil, false }
    if version == 0 && len(program) != 20 && len(program) != 32 { return 0, nil, false }
    return version, program, true
}
