// Package progress is the one line a chain scan prints while it is catching up:
// which blocks it has just built, how far through the catch-up that leaves it,
// how fast it is going and when it is likely to arrive.
//
// Every index in the bot resumes from a cursor and walks to the tip a chunk at a
// time (see blocks, miners, addrindex, addrstat and addrbal), and a first build
// of any of them is hours of work with nothing to say for itself. The line is
// printed at status level — verbosity 0 — because an operator who has started
// the bot to build an index has no other way to tell a long scan from a stuck
// one, and it stops as soon as the scan reaches the tip, where a rate and an ETA
// would be a forecast of work already done.
package progress

import "fmt"
import "time"
import "bitnsbot/logging"

// Progress is one catch-up: the height the scan resumed at, the tip it is
// heading for, and when it started. The rate is measured over the whole pass
// rather than the last chunk, since one slow chunk — a node busy connecting a
// block — says nothing about the hours left.
type Progress struct {
    name  string
    first int64
    tip   int64
    began time.Time
}

// New starts measuring a scan of blocks from..tip under the name it logs by.
func New(name string, from, tip int64) *Progress {
    return &Progress{name: name, first: from, tip: tip, began: time.Now()}
}

// Chunk reports that the scan has built blocks from..to, and says nothing once
// to has reached the tip: the catch-up is over, and each index logs the pass it
// has finished itself. The share is held under 100% while any block is left, a
// line that rounds to all of it being one an operator reads as finished.
func (p *Progress) Chunk(from, to int64) {
    if p == nil || to >= p.tip || to < p.first { return }
    var done = to - p.first + 1
    var left = p.tip - to
    var pct = float64(done) / float64(done+left) * 100
    if pct > 99.9 { pct = 99.9 }
    var rate = float64(done) / time.Since(p.began).Seconds()
    var eta = time.Duration(0)
    if rate > 0 { eta = time.Duration(float64(left) / rate * float64(time.Second)) }
    logging.Status("%s: built blocks %d..%d of %d, %.1f%% of %d to go, %s blocks/sec, ETA %s",
        p.name, from, to, p.tip, pct, left, trim(rate), wait(eta))
}

// trim prints a rate the way a reader of it cares: a decimal under ten blocks a
// second, where the difference between 0.4 and 0.9 is half the ETA, and whole
// blocks above it, where it is noise.
func trim(rate float64) string {
    if rate < 10 { return fmt.Sprintf("%.1f", rate) }
    return fmt.Sprintf("%.0f", rate)
}

// wait names a forecast with the two largest units that matter and no more, a
// scan of hours not being accurate to the second.
func wait(d time.Duration) string {
    if d >= time.Hour { return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d/time.Minute)%60) }
    if d >= time.Minute { return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d/time.Second)%60) }
    return fmt.Sprintf("%ds", int(d/time.Second))
}
