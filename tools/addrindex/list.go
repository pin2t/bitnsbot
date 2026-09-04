package main

import "context"
import "encoding/hex"
import "fmt"
import "os"
import "strconv"
import "strings"
import "time"

import "bitnsbot/addrindex"
import "bitnsbot/logging"

// entry is one line of the listing: a transaction the index says touched the
// address, resolved to its time, id and net effect.
type entry struct {
    at       int64
    txid     string
    received int64
    sent     int64
}

// net is what the address gained or lost, so a column of these sums to the
// balance. A transaction that both spends from the address and pays change back
// shows only the difference, which is what actually moved.
func (e entry) net() int64 { return e.received - e.sent }

// summary is what the totals line reports, accumulated over the entries.
type summary struct {
    received int64
    sent     int64
    count    int
    first    int64
    last     int64
}

func (s *summary) add(e entry) {
    s.received += e.received
    s.sent += e.sent
    s.count++
    if e.at > 0 {
        if s.first == 0 || e.at < s.first { s.first = e.at }
        if e.at > s.last { s.last = e.at }
    }
}

func (s summary) String() string {
    var out = fmt.Sprintf("Summary: Balance %d sats, Received %d sats, Sent %d sats, Transactions %d",
        s.received-s.sent, s.received, s.sent, s.count)
    if s.first > 0 {
        out += fmt.Sprintf(", Activity: from %s till %s", day(s.first), day(s.last))
    }
    return out
}

// list resolves every touch the index holds for an address and prints one line
// each, then the totals. The index stores (height, tx index) rather than txids —
// that is what makes it 10 bytes a touch — so each one costs a block lookup and
// a transaction lookup against the node. The touches come from the bbolt index
// this tool builds, or, with -dbsqlite, from the SQLite copy tosqlite makes of
// it; only the storage differs, and the listing is the same either way.
func list(opt *options, address string) {
    var client, err = newRPC(opt.url, opt.user, opt.pass, opt.cookie)
    if err != nil { logging.Fatal("RPC client: %v", err) }
    var ctx = context.Background()
    var scriptHex, serr = client.scriptOf(ctx, address)
    if serr != nil { logging.Fatal("%v", serr) }
    var script, derr = hex.DecodeString(scriptHex)
    if derr != nil { logging.Fatal("decode scriptPubKey: %v", derr) }

    var touches []addrindex.Touch
    var capped bool
    if opt.dbsqlite != "" {
        // the SQLite copy carries no cursor bucket, so there is nothing to warn
        // about: an index migrated at all was an index that had been built
        var err error
        touches, capped, err = sqliteTouches(opt.dbsqlite, script, opt.limit)
        if err != nil { logging.Fatal("%s: %v", opt.dbsqlite, err) }
    } else {
        if _, ok := addrindex.Cursor(); !ok {
            fmt.Fprintln(os.Stderr, "warning: this index has never been built — run addrindex build first")
        }
        touches, capped = addrindex.Lookup(script, opt.limit)
    }
    if capped {
        fmt.Fprintf(os.Stderr, "warning: stopped at -limit %d touches; the history is longer\n", opt.limit)
    }
    if len(touches) == 0 {
        fmt.Printf("No transactions in the index for %s\n", address)
        return
    }

    // Resolve first, print after: the amount column is right-aligned to the
    // widest value, which is not known until every entry is in.
    var entries []entry
    var txids = map[uint32][]string{}
    for _, t := range touches {
        ids, ok := txids[t.Height]
        if !ok {
            var fetched, err = client.txidsAt(ctx, t.Height)
            if err != nil {
                fmt.Fprintf(os.Stderr, "block %d: %v\n", t.Height, err)
                continue
            }
            txids[t.Height], ids = fetched, fetched
        }
        if int(t.TxIndex) >= len(ids) {
            fmt.Fprintf(os.Stderr, "block %d: no transaction at index %d\n", t.Height, t.TxIndex)
            continue
        }
        var tx, terr = client.transaction(ctx, ids[t.TxIndex])
        if terr != nil {
            fmt.Fprintf(os.Stderr, "tx %s: %v\n", shortID(ids[t.TxIndex]), terr)
            continue
        }
        var received, sent = tx.moved(address)
        entries = append(entries, entry{at: tx.Time, txid: tx.Txid, received: received, sent: sent})
    }

    var totals summary
    var width int
    for _, e := range entries {
        totals.add(e)
        if n := len(amount(e.net())); n > width { width = n }
    }
    for _, e := range entries {
        // the stamp is padded because a single-digit day is a character
        // shorter, which would step the whole listing in and out
        fmt.Printf("%-17s %s   %*s\n", stamp(e.at), shortID(e.txid), width, amount(e.net()))
    }
    fmt.Println(totals)
}

// amount is a signed satoshi figure. The sign is what makes the column readable
// as a ledger: a spend is a negative line, and they sum to the balance.
func amount(sat int64) string { return strconv.FormatInt(sat, 10) + " sat" }

// stamp and day render times the way the listing shows them: lowercase, day
// first. A transaction line carries the clock time, the activity range does not.
func stamp(unix int64) string {
    if unix <= 0 { return "unconfirmed" }
    return strings.ToLower(time.Unix(unix, 0).UTC().Format("2 Jan 2006 15:04"))
}

func day(unix int64) string {
    return strings.ToLower(time.Unix(unix, 0).UTC().Format("2 Jan 2006"))
}

// shortID abbreviates a txid to the two ends that identify it at a glance.
func shortID(s string) string {
    if len(s) <= 18 { return s }
    return s[:8] + ".." + s[len(s)-8:]
}
