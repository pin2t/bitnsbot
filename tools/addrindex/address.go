package main

import "crypto/sha256"

// This file turns a scriptPubKey into the address it pays, locally. The bot
// never does this — it compares raw scripts and lets the node handle address
// formats — but a scan that reads Core's files has no node to ask, and asking
// one per address was the last thing tying this pass to the RPC interface.
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

// scriptAddress returns the address a scriptPubKey pays to, or "" when it pays
// to none — an OP_RETURN, a bare multisig, anything nonstandard. These are the
// same forms Core's decodescript reports an address for.
func scriptAddress(script []byte) string {
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
        return base58Check(p2pkhVersion, hash160(script[1:34]))
    case len(script) == 67 && script[0] == 65 && script[66] == 0xac:
        return base58Check(p2pkhVersion, hash160(script[1:66]))
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

// hash160 is RIPEMD-160 of SHA-256, which is how a public key becomes the 20
// bytes an address carries.
func hash160(b []byte) []byte {
    var sum = sha256.Sum256(b)
    return ripemd160(sum[:])
}
