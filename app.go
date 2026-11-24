package main

import "context"
import "fmt"
import "math"
import "net/url"
import "strconv"
import "strings"
import "time"
import "bitnsbot/app"
import "bitnsbot/blocks"
import "bitnsbot/core"
import "bitnsbot/logging"
import "bitnsbot/miners"
import "bitnsbot/mp"
import "bitnsbot/rates"
import "bitnsbot/txwatches"
import "bitnsbot/watches"

// appSource is the data a Mini App renders: main's implementation of app.Source,
// adapting the fee cache, the database and the formatters to it so the Mini Apps
// stay unaware of Bitcoin Core, the price feeds and the caches. Telegram's app and
// the MAX one each get one, so the two show the same chain. platform is the one
// the app is on — its watch list, its watch button and its aliases act on that
// platform's watches, and a watch added from it is notified through it.
type appSource struct {
    platform string
}

// Network is the chain card, as the background refresh last cached it.
func (s appSource) Network() app.Network {
    networkMu.Lock()
    defer networkMu.Unlock()
    return cachedNetwork
}

// blocksPerPage is how many rows one batch of the Blocks tab carries.
const blocksPerPage = 12

// blocksMaxRows bounds a list restored by Back, which is as deep as the reader
// had scrolled. Twenty batches is further than anyone scrolls, and it is what
// stops an edited down= from asking for every block ever cached.
const blocksMaxRows = blocksPerPage * 20

// Blocks reads one window of the recent-block list straight out of the blocks
// table — `order by height desc`, which is the primary key read backwards, so
// there is no sorting and no node round trip, and `where height < ?` starts a
// batch at a given block without stepping over the ones above it.
//
// It asks for one row more than it returns: whether there is another batch below
// this one is what the trailing sentinel is rendered on, and a count of the whole
// table would be a scan of it.
//
// Down asks for every row back to the block a reader had scrolled to, which
// is why the limit is the deeper one above rather than a batch.
//
// A restore renders down to the row that was tapped; that there are more
// below it is what keeps the list's own sentinel alive.
func (s appSource) Blocks(lang string, rng app.Range) app.Blocks {
    var out = app.Blocks{Top: rng.After}
    if db == nil { return out }
    var limit = blocksPerPage
    if rng.Down > 0 { limit = blocksMaxRows }
    var where, args = "", []any{}
    if rng.Before > 0 {
        where, args = " where height < ?", []any{rng.Before}
    }
    if rng.After > 0 {
        where, args = " where height > ?", []any{rng.After}
    }
    if rng.Down > 0 {
        where, args = " where height >= ?", []any{rng.Down}
    }
    var rows, err = db.Query("select "+blocks.Columns+" from blocks"+where+
        " order by height desc limit ?", append(args, limit+1)...)
    if err != nil {
        logging.Warn("mini app: blocks: %v", err)
        return out
    }
    defer rows.Close()
    for rows.Next() {
        if len(out.Rows) == limit {
            out.More = true
            break
        }
        var bi, ok = blocks.Scan(rows)
        if !ok { continue }
        out.Rows = append(out.Rows, app.Block{
            Height:     group(bi.Height),
            Num:        bi.Height,
            Size:       humSize(int64(bi.Size), 2, lang),
            Txs:        i18nl(lang).Sprintf("%s txs", group(int64(bi.NumTx))),
            Miner:      minerName(bi.Miner, lang),
            MinerKnown: bi.Miner != "",
        })
    }
    if rng.Down > 0 && !out.More {
        var below int64
        out.More = db.QueryRow("select height from blocks where height < ? limit 1", rng.Down).Scan(&below) == nil
    }
    if len(out.Rows) > 0 {
        out.Top, out.Next = out.Rows[0].Num, out.Rows[len(out.Rows)-1].Num
    }
    out.OK = len(out.Rows) > 0
    return out
}

// BlockInfo backs the Mini App's block details page. It loads or computes the
// same record /info uses and renders the same lines from it, in the reader's
// language — which arrives with the request rather than from a chat, since the
// page has no chat behind it. A computed record is not stored: the collector is
// the only thing that fills the cache.
//
// Title is set even when the lookup fails, so the page still names what was
// asked for rather than reading "Block ". Kind says what this page is, which
// the app cannot always tell from the URL: a block hash has a txid's shape,
// so it arrives at the transaction endpoint and lands here — and a block has
// nothing to watch.
func (s appSource) BlockInfo(lang string, height int64) app.Info {
    var out = app.Info{Title: i18nl(lang).String("Block") + " " + group(height), Kind: "block"}
    var bi, ok = blocks.Load(height)
    if !ok {
        if !core.Enabled() { return out }
        var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
        defer cancel()
        var hash, err = core.GetBlockHash(ctx, height)
        if err != nil { return out }
        bi, err = blocks.Compute(ctx, hash)
        if err != nil {
            logging.Warn("mini app: compute block %d: %v", height, err)
            return out
        }
    }
    out.OK = true
    out.Rows = linkFields(blockPairs(bi, lang), []linked{{short(bi.Hash), bi.Hash}})
    if bi.Miner != "" {
        for i := range out.Rows {
            if out.Rows[i].Label != i18nl(lang).String("Miner") { continue }
            out.Rows[i].Parts = []app.Part{{Text: bi.Miner,
                Href: "miner?name=" + url.QueryEscape(bi.Miner) + "&down=" + strconv.FormatInt(bi.Height, 10)}}
        }
    }
    return out
}

// TxInfo backs the transaction details page. A block hash has the same 64-hex
// shape as a txid and only the node can tell them apart, so — exactly as info()
// does for the bot — an id the node has a block header for is shown as that
// block instead. Both live on the Blocks tab, so the handoff is seamless.
//
// the same ids the bot gives buttons for, which is already only what the
// text shows: the block it confirmed in, and the addresses on either side
//
// the app draws the input and output addresses as a two-sided flow under the
// fields rather than as two more rows, so those lines are dropped here — the
// bot's reply keeps them
//
// a block is named by height there, as "#963268"
func (s appSource) TxInfo(lang, txid string) app.Info {
    var out = app.Info{Title: short(txid)}
    if !core.Enabled() { return out }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    if header, err := core.GetBlockHeader(ctx, txid); err == nil {
        return s.BlockInfo(lang, header.Height)
    }
    var d, ok = txPairs(ctx, lang, txid)
    if !ok { return out }
    var links []linked
    for _, id := range d.ids {
        if _, err := strconv.ParseInt(id, 10, 64); err == nil {
            links = append(links, linked{"#" + id, id})
            continue
        }
        links = append(links, linked{short(id), id})
    }
    var rows [][2]string
    for _, p := range d.pairs {
        if p[0] == i18nl(lang).String("Inputs") || p[0] == i18nl(lang).String("Outputs") { continue }
        rows = append(rows, p)
    }
    return app.Info{OK: true, Title: short(d.canonical), Confirmed: d.confirmed,
        Rows: linkFields(rows, links), Inputs: d.inputs, Outputs: d.outputs,
        InCount: d.inCount, OutCount: d.outCount}
}

// AddrInfo backs the address details page. An input that is not an address at
// all says so, rather than reporting an empty history as fact.
//
// no core check here: an address the statistics collector follows is
// answered out of the database, which is the whole point of gathering it
func (s appSource) AddrInfo(lang, addr string) app.Info {
    var out = app.Info{Title: short(addr)}
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    var pairs, valid, err = addrPairs(ctx, lang, addr)
    if err != nil {
        logging.Warn("mini app: address %s: %v", short(addr), err)
        return out
    }
    if !valid {
        out.Rows = []app.Field{{Label: i18nl(lang).String("this does not look like a Bitcoin address")}}
        return out
    }
    out.OK, out.Rows = true, appFields(pairs)
    var txs = s.AddrTxs(lang, addr, 0)
    if txs.OK { out.Txs = &txs }
    return out
}

// AddrTxs backs the transaction views under the address page: one batch of
// newest-first transactions, resolved from the address index the same way the
// stats are, but windowed so scrolling fetches a few at a time. from is how
// many views the reader has already been shown.
func (s appSource) AddrTxs(lang, addr string, from int) app.Txs {
    var out = app.Txs{Addr: addr, Next: from}
    if !core.Enabled() { return out }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    var views, more, ok = addressTxViews(ctx, lang, addr, from)
    if !ok { return out }
    out.OK, out.Rows, out.More = true, views, more
    out.Next = from + len(views)
    return out
}

// SetWatch backs the Mini Apps' watch button, with watching() answering its
// state. It goes through the same addWatch/removeWatch the /watch and /unwatch
// commands use, so a watch added from an app fires notifications exactly like
// one added from the chat.
//
// SetWatch adds or removes a watch and reports the state it ended in — which is
// what the button renders, so a refusal shows as unpushed rather than lying.
// platform is the one the app is on: the watch is filed under it, and the
// notifier goroutine adding one starts sends through it.
func (s appSource) SetWatch(chat int64, kind, id string, on bool) (bool, error) {
    var platform = s.platform
    if !on {
        var _, err = removeWatch(platform, chat, id)
        return false, err
    }
    if watching(platform, chat, id) { return true, nil }
    var full, err = atWatchLimit(platform, chat)
    if err != nil { return false, err }
    if full {
        logging.Info("mini app: rejected watch for %s chat %d: at the limit of %d", platform, chat, maxSubscriptionsPerChat)
        return false, nil
    }
    if err := addWatch(platform, chat, id, "", nil); err != nil { return false, err }
    return true, nil
}

// MinerInfo backs the miner details page, opened from a pool name in the block
// list. The figures and their formatting are /miners', so the two agree.
func (appSource) MinerInfo(lang, name string) app.Info {
    var out = app.Info{Title: name}
    var s, ok = miners.Get(name)
    if !ok { return out }
    var blocks = i18np(lang).Plural(int64(s.Blocks), "block")
    out.OK = true
    out.Rows = []app.Field{
        {Label: i18nl(lang).String("Blocks mined"), Value: blocks},
        {Label: i18nl(lang).String("Reward"), Value: trimNum(toBTC(s.Reward), 2) + " BTC"},
        {Label: i18nl(lang).String("Fees"), Value: trimNum(toBTC(s.Fees), 2) + " BTC"},
        {Label: i18nl(lang).String("Consumption"), Value: trimNum(s.ConsumptionGW, 1) + " GW"},
    }
    return out
}

// Watches backs the Watches tab: the calling user's own watches, and nobody
// else's. chat comes from the signed launch data, and the same chat-scoping the
// bot applies in watchesCmd is what keeps one user's list out of another's. Each
// Mini App lists its own platform's watches — Telegram's or MAX's — since a chat
// number means nothing without it.
func (s appSource) Watches(chat int64) app.Watches {
    var platform = s.platform
    var records, err = watches.List()
    if err != nil {
        logging.Err("mini app: list watches: %v", err)
        return app.Watches{}
    }
    var out = app.Watches{OK: true}
    for _, r := range records {
        if r.Platform != platform || r.Chat != chat { continue }
        out.Addresses = append(out.Addresses, app.Watch{Short: short(r.Address), Id: r.Address, Alias: r.Alias})
    }
    for _, e := range txwatches.For(platform, chat) {
        out.Txs = append(out.Txs, app.Watch{Short: short(e.Txid), Id: e.Txid, Alias: e.Alias})
    }
    return out
}

// appFields turns the bot's label/value pairs into the app's rows. The pairs are
// already in the reader's language — the builders take it — so this only changes
// their shape.
func appFields(pairs [][2]string) []app.Field { return linkFields(pairs, nil) }

// linked is an id a row mentions: the text it appears as, and the id a tap on it
// opens. The text is what the line actually shows — an address shortened by
// short(), or "#963268" where a transaction names its block — since that is what
// has to be found in the line to be made tappable.
type linked struct {
    text string
    id   string
}

// linkFields is appFields with the ids marked up: each value is cut into the ids
// it mentions and the text between them, which is what lets the app render an
// address inside an output list, or a block's hash, as a tap. The pairs are the
// bot's own and are not rewritten — a row mentioning no id keeps its whole value
// — so this changes nothing about what the bot sends.
func linkFields(pairs [][2]string, links []linked) []app.Field {
    var out = make([]app.Field, 0, len(pairs))
    for _, p := range pairs {
        out = append(out, app.Field{Label: p[0], Value: p[1], Parts: splitLinks(p[1], links)})
    }
    return out
}

// splitLinks cuts one value at the ids it mentions, earliest first so the parts
// come out in reading order. nil means the value mentions none, which is what
// tells the page to render the line as it stands.
func splitLinks(value string, links []linked) []app.Part {
    var parts []app.Part
    for {
        var at, hit = -1, linked{}
        for _, l := range links {
            if l.text == "" { continue }
            var i = strings.Index(value, l.text)
            if i >= 0 && (at < 0 || i < at) { at, hit = i, l }
        }
        if at < 0 { break }
        if at > 0 { parts = append(parts, app.Part{Text: value[:at]}) }
        parts = append(parts, app.Part{Text: hit.text, Id: hit.id})
        value = value[at+len(hit.text):]
    }
    if parts == nil { return nil }
    if value != "" { parts = append(parts, app.Part{Text: value}) }
    return parts
}

// Market reads the rate history straight from the database — cheap enough that
// the card needs no cache of its own, unlike fees and the chain stats.
func (s appSource) Market(lang string) app.Market {
    var now, ok = rates.Last()
    if !ok { return app.Market{} }
    var m = app.Market{OK: true, Price: rates.Price(now)}
    var periods = []struct {
        label string
        back  time.Duration
    }{
        {i18nl(lang).String("1 d"), 24 * time.Hour},
        {i18nl(lang).String("1 w"), 7 * 24 * time.Hour},
        {i18nl(lang).String("1 mo"), 30 * 24 * time.Hour},
        {i18nl(lang).String("3 mo"), 90 * 24 * time.Hour},
        {i18nl(lang).String("1 y"), 365 * 24 * time.Hour},
        {i18nl(lang).String("5 y"), 5 * 365 * 24 * time.Hour},
    }
    for _, p := range periods {
        var then, have = rates.At(time.Now().Add(-p.back))
        if !have || then <= 0 {
            m.Changes = append(m.Changes, app.Change{Label: p.label, Pct: "—", Neutral: true})
            continue
        }
        var pct = (now - then) / then * 100
        m.Changes = append(m.Changes, app.Change{Label: p.label,
            Pct: fmt.Sprintf("%+.1f%%", pct), Up: pct >= 0})
    }
    return m
}

func (s appSource) Fees() app.Fees {
    feesMu.Lock()
    var rec, ok, count = cachedFees, cachedFeesOK, cachedFeesCount
    feesMu.Unlock()
    if !ok { return app.Fees{OK: false} }
    var price, havePrice = rates.Last()
    var tier = func(rate float64) app.Tier {
        var t = app.Tier{Rate: trimNum(rate, 2)}
        if havePrice { t.USD = usd(int64(math.Round(rate*typicalTxVsize)), price) }
        return t
    }
    return app.Fees{OK: true, Fast: tier(rec.fastest), Hour: tier(rec.hour),
        Slow: tier(rec.minimum), TxCount: group(int64(count))}
}

// Mempool backs the Mini App's mempool page. A negative after is the whole
// page: the fields /mempool prints and the list as it stands. Anything else is
// what an open page prepends — the arrivals newer than after, and the listed
// ones mined since, whose rows it drops — with the fields as they stand now,
// which the page swaps in over its own, so its counts move with its list.
// Fields that cannot be read are left out of a prepend rather than replacing
// good ones with an error.
//
// More says the page's list has a hole (see mp.List), so the page is handed the
// whole list to replace its own with rather than a prepend that would leave a
// gap.
func (s appSource) Mempool(lang string, after int64) app.Mempool {
    var out = app.Mempool{OK: true}
    var rows, ok = mempoolFields(lang)
    if after < 0 {
        out.Rows, out.OK = rows, ok
    } else if ok {
        out.Rows = rows
    }
    var top, txs, mined, more = mp.List(after)
    out.Top, out.Gone, out.More = top, mined, more
    for _, e := range txs {
        out.Txs = append(out.Txs, app.MempoolTx{Id: e.Txid, Short: short(e.Txid),
            Amount: amountText(e.Amount, lang), At: e.At.Unix(), Ago: relative(e.At.Unix(), lang)})
    }
    if !out.More { return out }
    var whole = s.Mempool(lang, -1)
    whole.More = true
    return whole
}

// mempoolFields are the lines /mempool prints — size, transaction count, flow
// rate and the total its transactions move — read from memory, where the rawtx
// frames keep them current, so the page asks the node nothing.
func mempoolFields(lang string) ([]app.Field, bool) {
    if !core.Enabled() {
        return []app.Field{{Label: i18nl(lang).String("Bitcoin node connection is not configured")}}, false
    }
    var count, size, statsOK = mp.Stats()
    if !statsOK {
        return []app.Field{{Label: i18nl(lang).String("Sorry, something went wrong reading the mempool")}}, false
    }
    var pairs = [][2]string{
        {i18nl(lang).String("Size"),         humSize(size, 2, lang)},
        {i18nl(lang).String("Transactions"), group(count)},
    }
    var rate, rateOK, change, changeOK = mp.Flow(time.Now())
    if rateOK {
        var fr = i18nl(lang).Sprintf("%.1f tx/sec", rate)
        if changeOK { fr += fmt.Sprintf(" (%+.1f)", change) }
        pairs = append(pairs, [2]string{i18nl(lang).String("Flow rate"), fr})
    }
    var amount, _, ok = mp.Summary()
    if ok && count > 0 {
        pairs = append(pairs, [2]string{i18nl(lang).String("Total flow"), "~" + btcAmount(amount)})
    }
    return appFields(pairs), true
}

// Watching reports whether the reader's chat on this platform watches id.
func (s appSource) Watching(chat int64, kind, id string) bool { return watching(s.platform, chat, id) }

// SetAlias names a watch the reader already has — the second step of the app's
// two-step add (the bell files the watch, the dialog names it) and what the
// Watches tab's edit dialog calls. An empty alias never reaches it: the page
// treats it as "leave it alone", so clearing a name is not something a stray
// tap can do.
func (s appSource) SetAlias(chat int64, kind, id, alias string) (bool, error) {
    return setAlias(s.platform, chat, id, alias)
}

// MinerChart is the bar chart under a miner's details, as of now.
func (s appSource) MinerChart(lang, name, data, period string) app.Chart {
    return minerChart(lang, name, data, period, time.Now())
}
