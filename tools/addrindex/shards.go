package main

import "bufio"
import "encoding/binary"
import "fmt"
import "io"
import "os"
import "path/filepath"

// A whole-chain balance pass moves more value than fits anywhere at once: the
// chain creates about three billion outputs and spends nearly as many, over
// something like 1.3 billion distinct scripts. Neither the movements nor the
// running total fit in 8 GB, and SQLite cannot be the accumulator either —
// measured with modernc.org/sqlite on this hardware, the cheapest row it can
// write (an append to an unindexed table) costs 5.2 µs, and a real sorted merge
// of a batch into a ten-million-row table cost 24 µs a row, which over billions
// of movements is days.
//
// So the movements are written to disk instead, split into files by a hash of
// the script, and each file is added up on its own afterwards. A shard holds
// every movement of every script that hashes to it and nothing else, so summing
// one needs only that shard's scripts in memory — which is what bounds a
// chain-sized job to a flag rather than to the chain's size.
type shards struct {
    dir     string
    files   []*os.File
    writers []*bufio.Writer
    scratch [binary.MaxVarintLen64]byte
    times   bool
    records int64
    bytes   int64
}

// shardName is one shard's file. They are numbered rather than named after the
// hash range they hold, since nothing ever looks a script up in them — they are
// read start to finish.
func shardName(dir string, i int) string { return filepath.Join(dir, fmt.Sprintf("%04d.mv", i)) }

// newShards creates count empty shard files under dir. bufKB is the write buffer
// each one gets: the movements arrive interleaved, so without a buffer per shard
// every record would be its own small write to a different place on the disk.
func newShards(dir string, count, bufKB int) (*shards, error) {
    return openShards(dir, count, bufKB, false)
}

// newTimedShards is newShards with room in every record for when the movement
// happened, which is what ababuild needs and richbuild does not. It is a
// constructor rather than a flag on put because the two have to agree: a shard
// written with the time is unreadable without it, and the reader can only know
// from how the file was opened.
func newTimedShards(dir string, count, bufKB int) (*shards, error) {
    return openShards(dir, count, bufKB, true)
}

func openShards(dir string, count, bufKB int, times bool) (*shards, error) {
    if err := os.MkdirAll(dir, 0755); err != nil { return nil, err }
    var s = &shards{dir: dir, times: times}
    for i := 0; i < count; i++ {
        var f, err = os.Create(shardName(dir, i))
        if err != nil {
            s.close()
            return nil, err
        }
        s.files = append(s.files, f)
        s.writers = append(s.writers, bufio.NewWriterSize(f, bufKB<<10))
    }
    return s, nil
}

// put records that a script's balance moved by sat. The record is the script's
// length, the script, and the amount — varints, because a script is 22 to 34
// bytes and the amounts are mostly small, and three billion records pay for
// every byte saved.
func (s *shards) put(script string, sat int64) error { return s.putAt(script, sat, 0) }

// putAt is put carrying the timestamp a timed shard holds: the block time of the
// last movement this record covers, whichever side it happened on. Zero means no
// movement, which only a caller that writes one can produce.
func (s *shards) putAt(script string, sat, when int64) error {
    var w = s.writers[fnvHash(script)%uint64(len(s.writers))]
    var n = binary.PutUvarint(s.scratch[:], uint64(len(script)))
    if _, err := w.Write(s.scratch[:n]); err != nil { return err }
    if _, err := w.WriteString(script); err != nil { return err }
    var m = binary.PutVarint(s.scratch[:], sat)
    if _, err := w.Write(s.scratch[:m]); err != nil { return err }
    s.records++
    s.bytes += int64(n + len(script) + m)
    if !s.times { return nil }
    var k = binary.PutUvarint(s.scratch[:], uint64(when))
    if _, err := w.Write(s.scratch[:k]); err != nil { return err }
    s.bytes += int64(k)
    return nil
}

// each reads one shard back, calling f for every movement in it. The script it
// hands over is reused between calls, so a caller keeping one must copy it —
// which the aggregation does, since it keys a map by it.
func (s *shards) each(i int, f func(script []byte, sat int64) error) error {
    return s.eachAt(i, func(script []byte, sat, when int64) error { return f(script, sat) })
}

// eachAt is each with the timestamp putAt wrote. On a shard opened without it the
// time is zero, so the two readers are one and a caller takes what its records
// actually carry.
func (s *shards) eachAt(i int, f func(script []byte, sat, when int64) error) error {
    if err := s.writers[i].Flush(); err != nil { return err }
    var file, err = os.Open(shardName(s.dir, i))
    if err != nil { return err }
    defer file.Close()
    var r = bufio.NewReaderSize(file, 1<<20)
    var script = make([]byte, 64)
    for {
        var length, lerr = binary.ReadUvarint(r)
        if lerr == io.EOF { return nil }
        if lerr != nil { return fmt.Errorf("shard %d: %w", i, lerr) }
        if int(length) > cap(script) { script = make([]byte, length) }
        script = script[:length]
        if _, rerr := io.ReadFull(r, script); rerr != nil { return fmt.Errorf("shard %d: %w", i, rerr) }
        var sat, serr = binary.ReadVarint(r)
        if serr != nil { return fmt.Errorf("shard %d: %w", i, serr) }
        var when uint64
        if s.times {
            var terr error
            if when, terr = binary.ReadUvarint(r); terr != nil { return fmt.Errorf("shard %d: %w", i, terr) }
        }
        if err := f(script, sat, int64(when)); err != nil { return err }
    }
}

// done releases a shard's file once it has been added up, so the disk the run
// needs falls away as the aggregation proceeds rather than all at the end.
func (s *shards) done(i int) error {
    if s.files[i] == nil { return nil }
    var err = s.files[i].Close()
    s.files[i] = nil
    if rerr := os.Remove(shardName(s.dir, i)); err == nil { err = rerr }
    return err
}

// flush pushes every buffer to its file, which is what makes the records
// readable back. It is not a commit: an interrupted run leaves the shards
// half-written, and the next one starts them again from the stored height.
func (s *shards) flush() error {
    for _, w := range s.writers {
        if err := w.Flush(); err != nil { return err }
    }
    return nil
}

func (s *shards) close() {
    for i := range s.files {
        if s.files[i] == nil { continue }
        s.writers[i].Flush()
        s.files[i].Close()
        s.files[i] = nil
    }
}

// remove throws the whole working directory away, which is what happens both
// when the run finishes and when it fails — the records are worth nothing
// without the height they were gathered up to.
func (s *shards) remove() {
    s.close()
    os.RemoveAll(s.dir)
}

// fnvHash spreads scripts over the shards. It is not the index's Prefix: that
// one is SHA-256, which costs about 116 ns a script here, and this is called
// once per record with nothing riding on it but an even split — two scripts
// landing in the same shard is not a collision, it is the point. FNV-1a is
// written out rather than taken from hash/fnv so it can read a string without
// copying it to a byte slice first.
func fnvHash(script string) uint64 {
    var h uint64 = 14695981039346656037
    for i := 0; i < len(script); i++ {
        h ^= uint64(script[i])
        h *= 1099511628211
    }
    return h
}
