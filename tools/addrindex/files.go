package main

import "bufio"
import "encoding/binary"
import "fmt"
import "io"
import "os"
import "path/filepath"
import "sort"

import "bitnsbot/addrindex"

// magic is what precedes every block in a blk file — mainnet's network magic,
// which is also how the reader tells a real record from the zero padding Core
// leaves at the end of a preallocated file.
var magic = []byte{0xf9, 0xbe, 0xb4, 0xd9}

// maxBlock bounds a record's declared size, so a corrupt length cannot make the
// reader allocate wildly. Consensus caps a block at 4M weight units, which no
// serialization exceeds by much.
const maxBlock = 8 << 20

// blockFiles lists the blk*.dat files in a Core blocks directory, in the order
// their numbers run. Blocks are not in height order inside them and a stale
// block may sit alongside the chain — neither matters here, since this pass only
// needs to see every script the chain has ever paid to, in any order.
func blockFiles(dir string) ([]string, error) {
    var names, err = filepath.Glob(filepath.Join(dir, "blk*.dat"))
    if err != nil { return nil, err }
    if len(names) == 0 {
        return nil, fmt.Errorf("no blk*.dat files in %s", dir)
    }
    sort.Strings(names)
    return names, nil
}

// xorKey reads the obfuscation key Core writes beside its block files. Since
// v28 the contents of blk and rev files are XORed with this repeating key, so
// without it every record reads as garbage. An absent file means an older node
// that wrote them in the clear, which an all-zero key expresses.
func xorKey(dir string) ([]byte, error) {
    var key, err = os.ReadFile(filepath.Join(dir, "xor.dat"))
    if os.IsNotExist(err) { return make([]byte, 8), nil }
    if err != nil { return nil, err }
    if len(key) == 0 { return make([]byte, 8), nil }
    return key, nil
}

// blockReader walks one blk file, handing back each block in turn. The key is
// applied by absolute file offset, which is how Core obfuscates: byte i of the
// file is XORed with key[i % len(key)].
type blockReader struct {
    r      *bufio.Reader
    f      *os.File
    key    []byte
    offset int64
    buf    []byte
}

func openBlockFile(name string, key []byte) (*blockReader, error) {
    var f, err = os.Open(name)
    if err != nil { return nil, err }
    return &blockReader{r: bufio.NewReaderSize(f, 1<<20), f: f, key: key}, nil
}

func (b *blockReader) Close() error { return b.f.Close() }

// read fills p from the file and undoes the obfuscation over it.
func (b *blockReader) read(p []byte) error {
    if _, err := io.ReadFull(b.r, p); err != nil { return err }
    if len(b.key) > 0 {
        for i := range p {
            p[i] ^= b.key[(b.offset+int64(i))%int64(len(b.key))]
        }
    }
    b.offset += int64(len(p))
    return nil
}

// next returns the next block's bytes, or nil at the end of the written data.
// The slice is reused between calls, so a caller that keeps anything from it
// must copy first — the scan only reads scripts out of it, so it does not.
func (b *blockReader) next() ([]byte, error) {
    var header = make([]byte, 8)
    var err = b.read(header)
    if err == io.EOF || err == io.ErrUnexpectedEOF { return nil, nil }
    if err != nil { return nil, err }
    // Core preallocates each file, so the written records are followed by zeros;
    // anything that is not the magic means this file is done.
    if string(header[:4]) != string(magic) { return nil, nil }
    var size = binary.LittleEndian.Uint32(header[4:])
    if size == 0 || size > maxBlock {
        return nil, fmt.Errorf("block at offset %d declares %d bytes", b.offset-8, size)
    }
    if cap(b.buf) < int(size) { b.buf = make([]byte, size) }
    b.buf = b.buf[:size]
    if err := b.read(b.buf); err != nil {
        // a half-written record at the end of the newest file is not an error,
        // it is simply where the chain currently stops
        if err == io.EOF || err == io.ErrUnexpectedEOF { return nil, nil }
        return nil, err
    }
    return b.buf, nil
}

// parseBlockOutputs reads a serialized block (80-byte header, then the
// transaction count, then each transaction) and returns each transaction's
// outputs — script and amount — indexed by the transaction's position in the
// block. It skips everything else — inputs, witness data, locktime — since a
// serialized block carries no prevouts to read a spend from. The transaction
// boundaries are kept because actbuild counts transactions, not outputs: an
// address paid twice by one transaction was involved in one transaction.
//
// It lives here rather than in the addrindex package because these files are the
// one place a block is binary. Everything else reads blocks over RPC, already
// decoded into an addrindex.Block.
func parseBlockOutputs(raw []byte) ([][]addrindex.Payment, bool) {
    var r = &reader{buf: raw}
    r.skip(80) // block header
    var txCount, ok = r.varInt()
    if !ok { return nil, false }
    var result = make([][]addrindex.Payment, txCount)
    for i := uint64(0); i < txCount; i++ {
        var scripts, txOK = skipTxKeepOutputs(r)
        if !txOK { return nil, false }
        result[i] = scripts
    }
    if r.bad { return nil, false }
    return result, true
}

func skipTxKeepOutputs(r *reader) ([]addrindex.Payment, bool) {
    r.skip(4) // version
    var inCount, ok = r.varInt()
    if !ok { return nil, false }
    var segwit bool
    if inCount == 0 { // segwit marker; the real input count follows the flag byte
        segwit = true
        r.skip(1)
        inCount, ok = r.varInt()
        if !ok { return nil, false }
    }
    for i := uint64(0); i < inCount; i++ {
        r.skip(36) // prevout hash + index
        var scriptLen, lenOK = r.varInt()
        if !lenOK { return nil, false }
        r.skip(int(scriptLen))
        r.skip(4) // sequence
    }
    var outCount, outOK = r.varInt()
    if !outOK { return nil, false }
    var scripts = make([]addrindex.Payment, 0, outCount)
    for i := uint64(0); i < outCount; i++ {
        var sat, satOK = r.value()
        if !satOK { return nil, false }
        var scriptLen, lenOK = r.varInt()
        if !lenOK { return nil, false }
        var script, scriptOK = r.bytes(int(scriptLen))
        if !scriptOK { return nil, false }
        scripts = append(scripts, addrindex.Payment{Script: script, Sat: sat})
    }
    if segwit {
        for i := uint64(0); i < inCount; i++ {
            var itemCount, itemOK = r.varInt()
            if !itemOK { return nil, false }
            for j := uint64(0); j < itemCount; j++ {
                var itemLen, ilOK = r.varInt()
                if !ilOK { return nil, false }
                r.skip(int(itemLen))
            }
        }
    }
    r.skip(4) // locktime
    return scripts, true
}

// reader and its varInt are a block-scale copy of the primitives the bot's
// zmq.go uses for a single mempool transaction. The two are separate programs
// reading different things — inputs and outputs for live matching there, outputs
// only here — so they are not shared.
type reader struct {
    buf []byte
    pos int
    bad bool
}

func (r *reader) skip(n int) {
    if n < 0 || r.pos+n > len(r.buf) { r.bad = true; return }
    r.pos += n
}

func (r *reader) bytes(n int) ([]byte, bool) {
    if n < 0 || r.pos+n > len(r.buf) { r.bad = true; return nil, false }
    var out = r.buf[r.pos : r.pos+n]
    r.pos += n
    return out, true
}

// value reads an output's amount, the 8-byte little-endian satoshi field every
// TxOut carries in front of its script.
func (r *reader) value() (int64, bool) {
    var b, ok = r.bytes(8)
    if !ok { return 0, false }
    return int64(binary.LittleEndian.Uint64(b)), true
}

func (r *reader) varInt() (uint64, bool) {
    var first, ok = r.bytes(1)
    if !ok { return 0, false }
    switch first[0] {
    case 0xfd:
        var b, ok = r.bytes(2)
        if !ok { return 0, false }
        return uint64(binary.LittleEndian.Uint16(b)), true
    case 0xfe:
        var b, ok = r.bytes(4)
        if !ok { return 0, false }
        return uint64(binary.LittleEndian.Uint32(b)), true
    case 0xff:
        var b, ok = r.bytes(8)
        if !ok { return 0, false }
        return binary.LittleEndian.Uint64(b), true
    default:
        return uint64(first[0]), true
    }
}
