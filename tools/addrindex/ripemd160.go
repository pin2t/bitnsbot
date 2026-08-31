package main

import "encoding/binary"
import "math/bits"

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

func ripemd160(msg []byte) []byte {
    var h = [5]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476, 0xc3d2e1f0}
    // the same padding MD5 and SHA-1 use: a 1 bit, zeros, then the length in
    // bits as a little-endian 64-bit count
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
        // the two lines are combined across the state, not into it
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
