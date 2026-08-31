package main

import "bufio"
import "encoding/binary"
import "fmt"
import "io"
import "os"
import "path/filepath"
import "sort"

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
