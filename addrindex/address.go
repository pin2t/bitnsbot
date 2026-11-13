package addrindex

import "bytes"
import "crypto/sha256"
import "encoding/binary"
import "math/bits"
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
//
// P2PKH: OP_DUP OP_HASH160 <20> OP_EQUALVERIFY OP_CHECKSIG
//
// P2SH: OP_HASH160 <20> OP_EQUAL
//
// P2PK: <pubkey> OP_CHECKSIG — no hash in the script, so the key is hashed
func Address(script []byte) string {
    switch {
    case len(script) == 25 && script[0] == 0x76 && script[1] == 0xa9 && script[2] == 20 &&
        script[23] == 0x88 && script[24] == 0xac:
        return base58Check(p2pkhVersion, script[3:23])
    case len(script) == 23 && script[0] == 0xa9 && script[1] == 20 && script[22] == 0x87:
        return base58Check(p2shVersion, script[2:22])
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
//
// OP_1 .. OP_16
//
// v0 is only ever a 20-byte key hash or a 32-byte script hash
func witness(script []byte) (version byte, program []byte, ok bool) {
    if len(script) < 4 || len(script) > 42 { return 0, nil, false }
    var op = script[0]
    switch {
    case op == 0x00:
        version = 0
    case op >= 0x51 && op <= 0x60:
        version = op - 0x50
    default:
        return 0, nil, false
    }
    var n = int(script[1])
    if n < 2 || n > 40 || len(script) != n+2 { return 0, nil, false }
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

// long division of the whole number by 58, which is what the encoding is
//
// a leading zero byte is not a digit but a character in its own right
func base58(b []byte) string {
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
    case p2pkhVersion: return string(append([]byte{keyP2PKH}, payload...)), "p2pkh", true
    case p2shVersion:  return string(append([]byte{keyP2SH}, payload...)), "p2sh", true
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

// RIPEMD-160, hand-rolled because Go's standard library does not carry it and
// the only maintained implementation is in golang.org/x/crypto, which would be a
// fourth dependency for one hash. It is needed for exactly one case — turning a
// bare public key into the 20 bytes a P2PK output's address is written from —
// and is pinned against the published vectors in the tests.

// the message word each round reads, in the left and right lines
var rmdLeft = [80]int{
    0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
    7, 4, 13, 1, 10, 6, 15, 3, 12, 0, 9, 5, 2, 14, 11, 8,
    3, 10, 14, 4, 9, 15, 8, 1, 2, 7, 0, 6, 13, 11, 5, 12,
    1, 9, 11, 10, 0, 8, 12, 4, 13, 3, 7, 15, 14, 5, 6, 2,
    4, 0, 5, 9, 7, 12, 2, 10, 14, 1, 3, 8, 11, 6, 15, 13,
}

var rmdRight = [80]int{
    5, 14, 7, 0, 9, 2, 11, 4, 13, 6, 15, 8, 1, 10, 3, 12,
    6, 11, 3, 7, 0, 13, 5, 10, 14, 15, 8, 12, 4, 9, 1, 2,
    15, 5, 1, 3, 7, 14, 6, 9, 11, 8, 12, 2, 10, 0, 4, 13,
    8, 6, 4, 1, 3, 11, 15, 0, 5, 12, 2, 13, 9, 7, 10, 14,
    12, 15, 10, 4, 1, 5, 8, 7, 6, 2, 13, 14, 0, 3, 9, 11,
}

// how far each round rotates, in the left and right lines
var rmdShiftLeft = [80]uint{
    11, 14, 15, 12, 5, 8, 7, 9, 11, 13, 14, 15, 6, 7, 9, 8,
    7, 6, 8, 13, 11, 9, 7, 15, 7, 12, 15, 9, 11, 7, 13, 12,
    11, 13, 6, 7, 14, 9, 13, 15, 14, 8, 13, 6, 5, 12, 7, 5,
    11, 12, 14, 15, 14, 15, 9, 8, 9, 14, 5, 6, 8, 6, 5, 12,
    9, 15, 5, 11, 6, 8, 13, 12, 5, 12, 13, 14, 11, 8, 5, 6,
}

var rmdShiftRight = [80]uint{
    8, 9, 9, 11, 13, 15, 15, 5, 7, 7, 8, 11, 14, 14, 12, 6,
    9, 13, 15, 7, 12, 8, 9, 11, 7, 7, 12, 7, 6, 15, 13, 11,
    9, 7, 15, 11, 8, 6, 6, 14, 12, 13, 5, 14, 13, 13, 7, 5,
    15, 5, 8, 11, 14, 14, 6, 14, 6, 9, 12, 9, 12, 5, 15, 8,
    8, 5, 12, 9, 12, 5, 14, 6, 8, 13, 6, 5, 15, 13, 11, 11,
}

var rmdKLeft = [5]uint32{0x00000000, 0x5a827999, 0x6ed9eba1, 0x8f1bbcdc, 0xa953fd4e}
var rmdKRight = [5]uint32{0x50a28be6, 0x5c4dd124, 0x6d703ef3, 0x7a6d76e9, 0x00000000}

// rmdF is the round function for each group of sixteen; the right line runs them
// in the opposite order.
func rmdF(group int, x, y, z uint32) uint32 {
    switch group {
    case 0:
        return x ^ y ^ z
    case 1:
        return (x & y) | (^x & z)
    case 2:
        return (x | ^y) ^ z
    case 3:
        return (x & z) | (y & ^z)
    default:
        return x ^ (y | ^z)
    }
}

// the same padding MD5 and SHA-1 use: a 1 bit, zeros, then the length in
// bits as a little-endian 64-bit count
//
// the two lines are combined across the state, not into it
func ripemd160(msg []byte) []byte {
    var h = [5]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476, 0xc3d2e1f0}
    var padded = make([]byte, 0, len(msg)+72)
    padded = append(padded, msg...)
    padded = append(padded, 0x80)
    for len(padded)%64 != 56 { padded = append(padded, 0) }
    var length = make([]byte, 8)
    binary.LittleEndian.PutUint64(length, uint64(len(msg))*8)
    padded = append(padded, length...)
    var x [16]uint32
    for block := 0; block < len(padded); block += 64 {
        for i := 0; i < 16; i++ {
            x[i] = binary.LittleEndian.Uint32(padded[block+i*4:])
        }
        var a, b, c, d, e = h[0], h[1], h[2], h[3], h[4]
        var aa, bb, cc, dd, ee = h[0], h[1], h[2], h[3], h[4]
        for i := 0; i < 80; i++ {
            var group = i / 16
            var t = bits.RotateLeft32(a+rmdF(group, b, c, d)+x[rmdLeft[i]]+rmdKLeft[group],
                int(rmdShiftLeft[i])) + e
            a, e, d, c, b = e, d, bits.RotateLeft32(c, 10), b, t
            t = bits.RotateLeft32(aa+rmdF(4-group, bb, cc, dd)+x[rmdRight[i]]+rmdKRight[group],
                int(rmdShiftRight[i])) + ee
            aa, ee, dd, cc, bb = ee, dd, bits.RotateLeft32(cc, 10), bb, t
        }
        var t = h[1] + c + dd
        h[1] = h[2] + d + ee
        h[2] = h[3] + e + aa
        h[3] = h[4] + a + bb
        h[4] = h[0] + b + cc
        h[0] = t
    }
    var out = make([]byte, 20)
    for i, v := range h { binary.LittleEndian.PutUint32(out[i*4:], v) }
    return out
}
