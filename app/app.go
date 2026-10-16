// Package app is what the two Mini Apps — Telegram's, in package telegram, and
// MAX's, in package max — share: the chain data they render, already formatted
// by main and handed over as one Options function per kind, and the events that
// tell them it moved (Notify, Listen).
//
// It holds no server of its own. Each Mini App is started by its platform's
// Start, which registers it with Listen.
package app

import "strings"
import "sync"
import "time"

// throttled are the events Notify holds to at most one per interval. The
// mempool list changes several times a second on mainnet, and each event makes
// every open page fetch; one every two seconds keeps the list live without
// that.
var throttled = map[string]time.Duration{"mempool": 2 * time.Second}

var throttleMu sync.Mutex

var throttleLast = map[string]time.Time{}

var throttlePending = map[string]bool{}

// throttle reports whether event may go out now. When it may not, it arranges
// for one to go out when the interval is up — a change that arrived inside the
// interval is announced late rather than lost — and every call until then folds
// into that one.
func throttle(event string) bool {
    var every, ok = throttled[event]
    if !ok { return true }
    throttleMu.Lock()
    defer throttleMu.Unlock()
    if throttlePending[event] { return false }
    var wait = every - time.Since(throttleLast[event])
    if wait <= 0 {
        throttleLast[event] = time.Now()
        return true
    }
    throttlePending[event] = true
    time.AfterFunc(wait, func() {
        throttleMu.Lock()
        throttlePending[event], throttleLast[event] = false, time.Now()
        throttleMu.Unlock()
        announce(event)
    })
    return false
}

// Notify tells both Mini Apps that a card's data changed. main calls it when a
// cache is actually refreshed, so the pages update when the numbers move rather
// than on a timer that mostly re-fetches the same values. Every function Listen
// registered hears it, on the caller's goroutine — a background refresh, which
// is why a listener must not block.
//
// An event in throttled goes out at most once per its interval.
func Notify(event string) {
    if throttle(event) { announce(event) }
}

func announce(event string) {
    listenMu.Lock()
    var heard = listeners
    listenMu.Unlock()
    for _, fn := range heard { fn(event) }
}

var listenMu sync.Mutex

var listeners []func(string)

// Listen has fn told of every event Notify announces, once it is past the
// throttling. telegram.Start and max.Start each register their Mini App's, so
// the two hear of the same changes without main notifying twice wherever
// something moves. fn is called on the announcing goroutine, so it must not
// block.
func Listen(fn func(event string)) {
    listenMu.Lock()
    defer listenMu.Unlock()
    listeners = append(listeners, fn)
}

// Tier is one fee estimate, already formatted for display: the sat/vB rate and
// the USD cost of a typical transaction (empty when no price is known).
type Tier struct {
    Rate string
    USD  string
}

// Fees is the fee card's data. OK is false when the background cache is cold, in
// which case the tiers are ignored and the card says so.
type Fees struct {
    OK      bool
    Fast    Tier
    Hour    Tier
    Slow    Tier
    TxCount string
}

// Network is the chain-at-a-glance card: circulating supply against the 21M cap,
// chain height, on-disk size and how many peers the node has heard from
// recently. Every field is already formatted; OK is false while the background
// cache is still cold.
type Network struct {
    OK        bool
    Coins     string
    Cap       string
    Blocks    string
    Size      string
    Nodes     string
    Txs       string
    Addresses string
}

// Change is one period's price move, already formatted: a short label, the
// signed percentage, and which way it went so the template can colour it. Up is
// meaningless when Pct is the placeholder — see Neutral.
type Change struct {
    Label   string
    Pct     string
    Up      bool
    Neutral bool
}

// Market is the price card: the current rate and how it has moved over each
// period. Changes always has one entry per period, so a gap in the rate history
// leaves a placeholder rather than collapsing the columns.
type Market struct {
    OK      bool
    Price   string
    Changes []Change
}

// Block is one row of the Blocks tab, already formatted: height, size,
// transaction count and the pool that mined it. MinerKnown is false when
// attribution failed and Miner is the "Unknown" placeholder, which is not a
// link — main owns that string, so the template does not compare against it.
type Block struct {
    // Height is the grouped display form ("963 268"); Num is the same height
    // raw, because the display form has spaces in it and the details link needs
    // something that survives a URL.
    Height     string
    Num        int64
    Size       string
    Txs        string
    Miner      string
    MinerKnown bool
}

// Field is one line of the block details page. An empty Value marks a heading
// ("Fees", "Tx sizes") rather than a field, which is how the bot's own reply is
// built — see blockPairs in main.
//
// Parts, when main sets them, is the same value cut into the ids it mentions and
// the text between them, so a reader can tap an address in a transaction's
// output list or a block's hash and land on that page. Value still carries the
// whole line: it is what a row without ids renders, and an empty one is still
// what marks a heading.
type Field struct {
    Label string
    Value string
    Parts []Part
}

// Part is one piece of a row's value. An empty Id is plain text; otherwise the
// part is tappable and Id is what it opens — handed to /search, which classifies
// it the way the bot's info() does, so a part needs to carry no kind of its own.
// Href is the alternative for a link /search cannot classify: a miner name opens
// the miner page outright, so the part carries the URL rather than an id.
type Part struct {
    Text string
    Id   string
    Href string
}

// FlowPart is one address on a side of a transaction's two-sided flow. Amount
// is the sats/BTC line shown under the address, with its dollar estimate
// in-line when a rate is known; empty when there is nothing to say.
type FlowPart struct {
    Text   string
    Id     string
    Amount string
}

// Info is a details page — a block, a transaction or an address. Title and Rows
// come from main, and are the same lines /info prints. Slot and Back are filled
// in by the handler: which container the fragment replaces, and where Back
// returns to, are the app's business rather than main's. OK is false when there
// is no such thing to show, or the node is not configured to look one up.
type Info struct {
    OK    bool
    Title string
    Rows  []Field
    Slot  string
    Back  string
    // Kind and Id name what the watch button acts on — "address" or "tx" and
    // the full id. Empty on a page with nothing to watch, which is what a block
    // or a miner page leaves them. An Options function may set Kind itself when the page it
    // returns is not what the URL asked for: a 64-hex id arrives at /tx, and one
    // the node has a block header for comes back as that block. The button
    // itself is loaded separately: whether *this* reader watches it is per-user,
    // and the page around it is one cached copy shared by all.
    Kind string
    Id   string
    // Confirmed marks a transaction already in a block. Watching a transaction
    // means waiting for it to confirm, so a confirmed one has nothing left to
    // watch for and the watch button stays off its page. Only a transaction
    // page sets it; an address is always watchable.
    Confirmed bool
    // Swap is the Back button's hx-swap. It carries a show: modifier when the
    // page was opened from the block list, which is what scrolls the row the
    // reader tapped back into view.
    Swap string
    // From is where a tapped id inside this page sends its own Back: the origin
    // this page was opened with, carried on so a whole chain of pages returns to
    // where the reader started rather than to whichever one they came through.
    From string
    // Chart is drawn under the rows when set, which only a miner page does. It
    // is filled in by the handler rather than by main's MinerInfo, since which
    // chart a page opens on is the app's choice.
    Chart *Chart
    // Inputs and Outputs are the two sides of a transaction page's flow, drawn
    // under the fields: the input addresses on the left, the output addresses
    // on the right and an arrow between them — the same layout an address
    // page's transaction cards use. Only a transaction page sets them.
    Inputs  []FlowPart
    Outputs []FlowPart
    // InCount and OutCount are how many inputs and outputs the transaction
    // has, named above each side of the flow — the sides themselves may be cut.
    InCount  int
    OutCount int
    // Txs is the transaction list drawn under the rows, newest first — only an
    // address page sets it. The first batch is part of the page itself; the
    // sentinel below the views appends the next batches as the reader scrolls.
    Txs *Txs
}

// Chart is the bar chart under a miner's details: one bar per day, week or month
// of the chosen period, every figure already formatted and every height already
// scaled by main. Name, Data and Period are carried back out because each button
// has to link to the same pool with one of the two selections changed.
type Chart struct {
    OK     bool
    Name   string
    // Data is "blocks" or "consumption", Period "month", "quarter" or "year".
    Data   string
    Period string
    // Value and Label are the headline above the bars — the whole period's figure
    // and what it covers ("412 blocks", "last 30 days"). A tapped bar shows its
    // own in their place, and they are what comes back when it is let go.
    Value string
    Label string
    // Top and Mid label the two gridlines, the scale's top and its half.
    Top string
    Mid string
    Bars []Bar
}

// Bar is one bucket of a Chart. Height is a percentage of the scale's top; Value
// and Label are what the headline shows when the bar is tapped; Tick is the axis
// label under it, empty for all but a few.
type Bar struct {
    Height float64
    Value  string
    Label  string
    Tick   string
}

// MempoolTx is one row of the live mempool list: the transaction, what its
// outputs pay in total, and when it arrived — At as a unix time the page keeps
// the relative time fresh from, Ago that time as the server first renders it.
type MempoolTx struct {
    Id     string
    Short  string
    Amount string
    At     int64
    Ago    string
}

// Mempool is the mempool page, or what an open one prepends. Rows are the
// fields /mempool prints, set only on the whole page. Txs are arrivals newest
// first, and Gone the listed ones a block has mined since, whose rows the page
// drops. Top is the sequence number the sentinel above the list asks for
// anything newer than; More says the page must replace its list rather than
// prepend to it. Lang and Back are filled in by the handler.
type Mempool struct {
    OK   bool
    Rows []Field
    Txs  []MempoolTx
    Gone []string
    Top  int64
    More bool
    Lang string
    Back string
}

// Blocks is one batch of the recent-block list, newest first. Top and Next are
// the heights the two sentinels ask about, so the template does no arithmetic.
type Blocks struct {
    OK   bool
    Rows []Block
    // Top is the highest height the list holds — what the sentinel above the
    // rows asks for anything newer than. On a batch that came back empty it is
    // the height that was asked about, so the sentinel keeps its place.
    Top int64
    // Next is the lowest height in this batch: where the sentinel below the rows
    // continues. More says whether the batch was cut short — there are older
    // blocks to append, or (on a newer-than request) more new ones than fit.
    Next int64
    More bool
}

// Range is the window of the block list a request wants. At most one field is
// set; all three zero is the newest batch, which is what the tab opens on.
type Range struct {
    // Before is the next batch as the reader scrolls: the blocks below this
    // height. It is a height rather than a page number because the list grows at
    // the head — an offset would shift under a reader mid-scroll and repeat the
    // row that crossed the boundary.
    Before int64
    // After is what a new block prepends: the blocks above this height.
    After int64
    // Down restores a list Back returns to: everything from the tip down to the
    // block the reader had opened, so the rows they had scrolled through are
    // there again.
    Down int64
}

// Addr is one row of an address list: the shortened form the row shows, the full
// address its link carries, and the one figure that list ranks by — a
// transaction count, a balance, or the date the coins last moved — already
// formatted in the reader's language.
//
// Idx is the row's offset in the list, which is the mark Back returns to. An
// offset is exact here in a way it could not be for blocks: these lists are
// static, so no row shifts under a reader mid-scroll.
type Addr struct {
    Short string
    Id    string
    Value string
    Idx   int
}

// Addrs is one batch of one address list. Kind is carried back out because every
// link the batch renders — the next sentinel, a row's details page, the buttons
// at the foot of the panel — has to name the list it belongs to.
type Addrs struct {
    OK   bool
    Kind string
    Rows []Addr
    // Next is the offset the sentinel below the rows continues at, and More
    // whether there is anything there to fetch.
    Next int
    More bool
}

// Tx is one transaction view on an address page: when it happened, how much it
// moved in total, and the addresses on both sides. Short is the shortened txid
// the card's title row shows, a tap on it opens the transaction's own page —
// the card's two sides are FlowParts, so an address is tappable exactly like a
// row of the details page; a part with no Id — a non-standard output, say — is
// plain text. The card's flow shows the addresses alone: the per-address
// amount lines belong to the transaction details page.
type Tx struct {
    Time    string
    Amount  string
    USD     string
    Id      string
    Short   string
    Inputs  []FlowPart
    Outputs []FlowPart
    // InCount and OutCount are the transaction's whole input and output
    // counts, drawn on the two edges of the card's arrow, since each side
    // stops at cardAddrs.
    InCount  int
    OutCount int
}

// Txs is one batch of an address's transaction views, newest first. Next is the
// offset the sentinel below the views continues at, and More whether there are
// older transactions to append. Slot and From are filled in by the handler, the
// same two values the details page carries, so a tapped address inside a view
// opens in the right container and Back still returns where the reader started.
type Txs struct {
    OK   bool
    Addr string
    Rows []Tx
    Next int
    More bool
    Slot string
    From string
}

// AddrRange is the window of an address list a request wants.
type AddrRange struct {
    // Kind is which list: "active", "rich" or "abandoned". Anything else has
    // already been turned into the default by the handler.
    Kind string
    // From is the offset the batch starts at — a plain offset, where the block
    // list needs a height, because nothing is ever inserted above these rows.
    From int
    // Down restores a list Back returns to: everything from the top down to the
    // row the reader had opened. Restore says whether one was asked for at all,
    // since row 0 is the top of the list and a legitimate answer, so the value
    // alone cannot mean both that and "not asked".
    Down    int
    Restore bool
}

// Watch is one watched id on the Watches tab: the shortened form the row shows,
// the full id its link carries, and the alias the user gave it, if any.
type Watch struct {
    Short string
    Id    string
    Alias string
}

// Watches is one user's watch list. Alone among the Options functions this one is
// per-user, which is why it is never cached and never rendered into the shell
// page — both of those are shared by every visitor. OK is false when the lookup
// failed, which is a different answer from watching nothing.
type Watches struct {
    OK        bool
    Addresses []Watch
    Txs       []Watch
}

// Options supplies the chain data a Mini App renders, one function per kind of
// data. main passes them to both; the Mini Apps stay unaware of the fee cache,
// Bitcoin Core and the price feeds.
//
// Everything a function renders text into takes the reader's language, because
// the words are main's rather than the page's: the details rows are the very
// lines the bot prints, and a block row's "Unknown" miner or a period's "1w" is a
// string main chose. The functions that carry no words of their own — the fee
// tiers and the chain figures are numbers, a watch list is ids — take none, and
// their labels come from the translated page around them.
type Options struct {
    Fees       func() Fees
    Network    func() Network
    Market     func(lang string) Market
    Blocks     func(lang string, rng Range) Blocks
    Addresses  func(lang string, rng AddrRange) Addrs
    BlockInfo  func(lang string, height int64) Info
    TxInfo     func(lang, txid string) Info
    AddrInfo   func(lang, address string) Info
    AddrTxs    func(lang, address string, from int) Txs
    MinerInfo  func(lang, name string) Info
    MinerChart func(lang, name, data, period string) Chart
    Mempool    func(lang string, after int64) Mempool
    Watches    func(chat int64) Watches
    Watching   func(chat int64, kind, id string) bool
    SetWatch   func(chat int64, kind, id string, on bool) (bool, error)
    SetAlias   func(chat int64, kind, id, alias string) (bool, error)
}

// 000000 prefix is likely a block hash not transaction id
func IsTxID(hash string) bool {
    if len(hash) != 64 { return false }
    for _, c := range hash {
        if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') { return false }
    }
    return !strings.HasPrefix(hash, "000000")
}
