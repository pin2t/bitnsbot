// Package signals is how one part of the bot tells another that there is work to
// do now, rather than at the end of its own interval.
//
// Every chain-derived index — the block cache, the address index, the miner
// statistics, the per-address statistics, the address balances — is kept current
// by one catch-up goroutine, which wakes on a block notification and on a timer
// of its own. That timer is the floor, not the cadence: a block arrives every
// ten minutes on average and the timers are ten minutes long, so the catch-up
// could sit a whole interval behind a block it already knows about. A signal is
// what closes that gap, and it is the same shape the app package's Notify uses
// for the same reason.
//
// A subscriber's channel holds **one** pending signal, sent without blocking. So
// a catch-up that is busy when three blocks arrive wakes once more when it
// finishes rather than three times over the same work, and a fire with nobody
// listening costs nothing.
package signals

import "sync"

// Block is fired when a block notification arrives from Core over ZMQ. The index
// catch-up goroutine and the Mini App's fee and network cards wait on it
// alongside their own timers.
const Block = "block"

// AddrStat is fired when the per-address statistics have caught up, which is
// what the three ranked address lists are built from — so they are rebuilt when
// the figures they rank actually move, not an hour later.
const AddrStat = "addrstat"

// AddrBal is fired when the address balances have moved or caught up, which is
// what the Blockchain card's address count reads — so a restart shows the count
// as soon as the scan has it, not at the next block.
const AddrBal = "addrbal"

var mu sync.Mutex
var subs = map[string][]chan struct{}{}

// Subscribe returns a channel that receives the named signal. Called once, at
// the top of the goroutine that waits on it.
func Subscribe(name string) <-chan struct{} {
    var ch = make(chan struct{}, 1)
    mu.Lock()
    subs[name] = append(subs[name], ch)
    mu.Unlock()
    return ch
}

// Send wakes everyone waiting on name. It never blocks, so it is safe to call
// from a notification handler: a subscriber that is already awake, or already
// has a signal pending, simply keeps the one it has.
func Send(name string) {
    mu.Lock()
    var chans = subs[name]
    mu.Unlock()
    for _, ch := range chans {
        select {
        case ch <- struct{}{}:
        default:
        }
    }
}

// Reset drops every subscription. For tests, which share this package.
func Reset() {
    mu.Lock()
    subs = map[string][]chan struct{}{}
    mu.Unlock()
}
