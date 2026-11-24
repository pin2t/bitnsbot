package main

import "context"
import "encoding/json"
import "flag"
import "fmt"
import "html"
import "net/http"
import "os"
import "os/signal"
import "strconv"
import "strings"
import "sync"
import "syscall"
import "time"
import "runtime/debug"
import "bitnsbot/blocks"
import "bitnsbot/addrbal"
import "bitnsbot/app"
import "bitnsbot/core"
import "bitnsbot/chats"
import "bitnsbot/cursors"
import "bitnsbot/addrstat"
import "bitnsbot/addrindex"
import "bitnsbot/logging"
import "bitnsbot/max"
import "bitnsbot/miners"
import "bitnsbot/mp"
import "bitnsbot/rates"
import "bitnsbot/signals"
import "bitnsbot/telegram"
import "bitnsbot/txwatches"
import "bitnsbot/watches"
import "unicode/utf8"
import "math"
import "regexp"
import "bitnsbot/lru"
import "sort"

var configPath      = flag.String("config", "", "path to the TOML configuration file")
var verbose         = flag.Int("verbose", 0, "log verbosity: 0=ERR/WARN/status, 1=+INFO, 2=+NET/DB (raw external traffic and storage requests)")

// stopBackup ends the backup goroutine, set when db.backup.path started one. shutdown
// runs it before closing the database, since a copy in flight is reading it.
var stopBackup func()

var ver = "1.4"
var commit = ""

// The statistics collector reads the same blocks on a pass of its own:
// the index says which transactions an address is in, where this says
// what they did to it, and the two advance at different rates.
//
// At least one of telegram.token and max.token is required, and each messenger is
// started only if its token is given. A first build of the chain-derived
// indexes, which is hours of work, is the tools' job (tools/addrindex,
// tools/addrstat), run against the same database file the bot then serves from.
func main() {
    var b, _ = debug.ReadBuildInfo()
    if b != nil {
        for _, s := range b.Settings {
            if s.Key == "vcs.revision" { commit = s.Value[:6] }
        }
    }
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
        flag.PrintDefaults()
    }
    flag.Parse()
    logging.SetVerbose(*verbose)
    var cfg, cfgErr = loadConfig(*configPath)
    if cfgErr != nil {
        logging.Fatal("config: %v", cfgErr)
    }
    if cfg.Telegram.Token == "" && cfg.Max.Token == "" {
        logging.Fatal("telegram.token or max.token is required")
    }
    if cfg.Telegram.Token != "" && cfg.Telegram.RegisterWebhook && cfg.Telegram.WebhookURL == "" {
        logging.Fatal("telegram.webhookURL is required when telegram.registerWebhook is true")
    }
    var err error
    if err = openDB(cfg.DB.Path); err != nil {
        logging.Fatal("open database: %v", err)
    }
    if err = cursors.Init(db); err != nil { logging.Fatal("open database: cursors: %v", err) }
    if err = chats.Init(db); err != nil { logging.Fatal("open database: chats: %v", err) }
    if err = blocks.Init(db); err != nil { logging.Fatal("open database: blocks: %v", err) }
    if err = rates.Init(db); err != nil { logging.Fatal("open database: rates: %v", err) }
    if err = watches.Init(db); err != nil { logging.Fatal("open database: watches: %v", err) }
    if err = miners.Init(db); err != nil { logging.Fatal("open database: miners: %v", err) }
    if err = addrstat.Init(db); err != nil { logging.Fatal("open database: addrstat: %v", err) }
    if err = addrbal.Init(db); err != nil { logging.Fatal("open database: addrbal: %v", err) }
    if err = addrindex.Init(db); err != nil { logging.Fatal("open database: addrindex: %v", err) }
    rates.Start()
    if cfg.DB.Backup.Path != "" {
        _, stopBackup = startBackup(cfg.DB.Backup.Path, cfg.DB.Backup.Interval, cfg.DB.Backup.Script)
    }
    if cfg.Core.URL != "" {
        if err := core.Init(cfg.Core.URL, cfg.Core.User, cfg.Core.Password, cfg.Core.Cookie); err != nil {
            logging.Fatal("Bitcoin Core client: %v", err)
        }
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var height, herr = core.GetBlockCount(ctx)
        cancel()
        if herr != nil {
            logging.Fatal("Bitcoin Core at %s: %v", cfg.Core.URL, herr)
        }
        logging.Status("connected to Bitcoin Core at %s (height %d)", cfg.Core.URL, height)
    }
    var platforms []string
    if cfg.Telegram.Token != "" {
        if err := telegram.Start(telegram.Options{
            Token:       cfg.Telegram.Token,
            APIBaseURL:  cfg.Telegram.API,
            Listen:      cfg.Telegram.Listen,
            WebhookURL:  cfg.Telegram.WebhookURL,
            Secret:      cfg.Telegram.Secret,
            Register:    cfg.Telegram.RegisterWebhook,
            Menus:       menus(),
            Message:     message,
            Callback:    callback,
            AppListen:   cfg.Telegram.App.Listen,
            App:         appSource{platform: telegram.Platform},
        }); err != nil {
            logging.Fatal("Telegram: %v", err)
        }
        platforms = append(platforms, telegram.Platform)
    }
    if cfg.Max.Token != "" {
        if err := max.Start(max.Options{
            Token:      cfg.Max.Token,
            Listen:     cfg.Max.Listen,
            WebhookURL: cfg.Max.WebhookURL,
            Secret:     cfg.Max.Secret,
            AppListen:  cfg.Max.App.Listen,
            App:        appSource{platform: max.Platform},
            Message:    message,
            Callback:   callback,
            Started:    started,
        }); err != nil {
            logging.Fatal("MAX: %v", err)
        }
        platforms = append(platforms, max.Platform)
    }
    miners.Start()
    startIndexUpdates()
    startNotify(platforms...)
    mp.Start()
    startMempoolSummary()
    startMempoolFees()
    startNetworkStats()
    startMarketUpdates()
    if core.Enabled() && len(cfg.Core.ZMQ) > 0 {
        if err := startZMQ(context.Background(), cfg.Core.ZMQ); err != nil {
            logging.Fatal("subscribe to Bitcoin Core ZMQ: %v", err)
        }
    }
    var ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    <-ctx.Done()
    stop()
    shutdown()
}

// shutdown tears down in dependency order: the webhook servers and the Mini
// Apps first, waiting for the handlers in flight, since those handlers use the
// database; then the notifiers and the backup, both before closeDB, so the
// database is not closed under a copy in flight or a rebuild that is writing
// to it.
func shutdown() {
    logging.Status("shutting down")
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if err := telegram.Shutdown(ctx); err != nil {
        logging.Err("Telegram shutdown: %v", err)
    }
    if err := max.Shutdown(ctx); err != nil {
        logging.Err("MAX shutdown: %v", err)
    }
    stopNotify()
    if stopBackup != nil { stopBackup() }
    if err := closeDB(); err != nil {
        logging.Err("close watches database: %v", err)
    }
}

// message runs a message typed in a chat on a platform — Telegram and MAX both
// hand theirs here, so a command behaves the same on either. The chat is
// recorded first (chats.Update) — its language among the rest, since every reply
// is translated through it — with body, the update as the platform sent it,
// kept if this is the chat's first.
func message(platform string, chat int64, name, text, lang string, body []byte) {
    if err := chats.Update(platform, chat, lang, body); err != nil { logging.Err("record chat %d: %v", chat, err) }
    logMessage(platform, chat, name, text)
    var command, arg = parseCommand(text)
    switch command {
    case "/start":
        start(platform, chat)
    case "/info":
        info(platform, chat, arg)
    case "/watch":
        watchCmd(platform, chat, arg, body)
    case "/unwatch":
        unwatch(platform, chat, arg)
    case "/watches":
        watchesCmd(platform, chat)
    case "/fees":
        fees(platform, chat)
    case "/mempool":
        mempoolCmd(platform, chat)
    case "/miners":
        minersCmd(platform, chat)
    case "/market":
        marketCmd(platform, chat)
    case "":
        var pending, err = chats.Take(platform, chat, chats.Info)
        if err != nil { logging.Err("chat %d state: %v", chat, err) }
        if pending {
            info(platform, chat, text)
            return
        }
        if pending, err = chats.Take(platform, chat, chats.Watch); err != nil { logging.Err("chat %d state: %v", chat, err) }
        if pending {
            watchCmd(platform, chat, text, body)
            return
        }
        if pending, err = chats.Take(platform, chat, chats.Unwatch); err != nil { logging.Err("chat %d state: %v", chat, err) }
        if pending {
            unwatch(platform, chat, text)
        }
    }
}

// callback runs a tapped id button. The button carries the full id, so the
// lookup is just /info on it.
func callback(platform string, chat int64, name, id, lang string, body []byte) {
    if err := chats.Update(platform, chat, lang, body); err != nil { logging.Err("record chat %d: %v", chat, err) }
    logMessage(platform, chat, name, id)
    info(platform, chat, id)
}

// started answers the bot being started in a chat — MAX tells of it as an
// update of its own, where Telegram sends /start as a message — with the start
// message.
func started(platform string, chat int64, name, lang string, body []byte) {
    if err := chats.Update(platform, chat, lang, body); err != nil { logging.Err("record chat %d: %v", chat, err) }
    logMessage(platform, chat, name, "/start")
    start(platform, chat)
}

// chatNames remembers who each chat last heard from, so a message sent to it —
// a reply, or a notification long after — is logged with a name as well as the
// chat id. Bounded like chatLangs, since anyone can message the bot.
var chatNames = lru.New[chatKey, string](10000)

// logLimit is how many characters of a message the log keeps.
const logLimit = 100

// logMessage logs a message received in chat from name, and remembers the name
// for what is sent back (logSent).
func logMessage(platform string, chat int64, name, text string) {
    chatNames.Put(chatKey{platform, chat}, name)
    logging.Info("bot (%s) <-- %s (%d): %s", platform, name, chat, logText(text))
}

// logSent logs a message sent to chat, under the name the chat was last heard
// from as, "unknown" when it has not been since the bot started.
func logSent(platform string, chat int64, text string) {
    var name, ok = chatNames.Get(chatKey{platform, chat})
    if !ok { name = "unknown" }
    logging.Info("bot (%s) --> %s (%d): %s", platform, name, chat, logText(text))
}

// logText is a message as one log line: its HTML tags dropped and entities
// decoded, since every reply is HTML, its line breaks made spaces, and cut to
// logLimit characters followed by "...". Characters, not bytes, so a Russian
// message is not cut inside a letter.
func logText(text string) string {
    var plain = html.UnescapeString(htmlTag.ReplaceAllString(text, ""))
    plain = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(plain)
    var runes = []rune(plain)
    if len(runes) > logLimit { return string(runes[:logLimit]) + "..." }
    return plain
}

// htmlTag matches one HTML tag, for logText to drop.
var htmlTag = regexp.MustCompile(`<[^>]*>`)

func parseCommand(text string) (command, arg string) {
    var fields = strings.SplitN(strings.TrimSpace(text), " ", 2)
    if !strings.HasPrefix(fields[0], "/") { return "", "" }
    command = strings.SplitN(fields[0], "@", 2)[0]
    if len(fields) > 1 { arg = strings.TrimSpace(fields[1]) }
    return command, arg
}

// commands is the single source for both /start's listing and the command menu
// Telegram shows beside the input field, so the two cannot drift apart. The menu
// description is the part of the line after the dash; keeping the whole /start
// line as the key means the existing translations keep working untouched.
var commands = []struct{ name, line string }{
    {"info", "• <b>/info</b> — look up a transaction, block, or address\n"},
    {"watch", "• <b>/watch</b> — get notified when an address receives a payment, or when a transaction confirms\n"},
    {"unwatch", "• <b>/unwatch</b> — stop watching an address or transaction\n"},
    {"watches", "• <b>/watches</b> — list what you're currently watching\n"},
    {"fees", "• <b>/fees</b> — show current network fee estimates\n"},
    {"mempool", "• <b>/mempool</b> — show current mempool size and totals\n"},
    {"miners", "• <b>/miners</b> — top mining pools by blocks mined\n"},
    {"market", "• <b>/market</b> — price, market cap, volume and recent changes\n"},
    {"start", "• <b>/start</b> — show this message\n"},
}

func start(platform string, chat int64) {
    send(platform, chat, startText(chatLang(platform, chat)), nil)
}

// startText is the /start reply in a language, shared by Telegram and MAX.
func startText(lang string) string {
    var text = i18nl(lang).String("<a href=\"https://bitnsbot.com\">Bitnsbot</a>. I keep an eye on the Bitcoin network for you.") + "\n\n"
    for _, c := range commands { text += i18nl(lang).String(c.line) }
    return text + "\n" +
        i18nl(lang).Sprintf("Version %s. Build %s. Source code <a href=\"https://github.com/pin2t/bitnsbot\">bitnsbot</a>. Don't forget to give me a ⭐",
            ver, commit)
}

// menuDescription is the text Telegram shows beside a command in the menu: the
// /start line without its bullet and command prefix.
func menuDescription(line string) string {
    var _, desc, _ = strings.Cut(line, " — ")
    return strings.TrimSuffix(desc, "\n")
}

// menus is the command menu Telegram shows beside the input field, one list
// per language. Telegram stores a separate list per language_code and falls back
// to the list registered without one, so English goes in under an empty code.
func menus() map[string][]telegram.Command {
    var langs = []string{""}
    for lang := range langTrans { langs = append(langs, lang) }
    var out = map[string][]telegram.Command{}
    for _, lang := range langs {
        for _, c := range commands {
            out[lang] = append(out[lang], telegram.Command{Command: c.name,
                Description: menuDescription(langTrans[lang].String(c.line))})
        }
    }
    return out
}

// chatKey names a chat on a platform: chatNames is keyed by it, since a chat
// number alone is ambiguous between Telegram and MAX.
type chatKey struct {
    platform string
    chat     int64
}

// maxSubscriptionsPerChat bounds how many subscriptions (address and transaction
// watches) a single chat may hold at once. watchCmd rejects a new watch once the
// chat already has this many.
const maxSubscriptionsPerChat = 500

// atWatchLimit reports whether this chat may add another watch. The cap counts
// both kinds together: each address watch costs a live goroutine and a channel
// in the notification fan-out, and every one is woken for every match.
func atWatchLimit(platform string, chat int64) (bool, error) {
    var count, err = watches.Count(platform, chat)
    if err != nil { return false, err }
    return count+len(txwatches.For(platform, chat)) >= maxSubscriptionsPerChat, nil
}

// addWatch records a watch and starts everything that makes it fire: the store
// or the in-memory list, the notifier goroutine, and the local script/outpoint
// matcher. /watch and the Mini App's bell both go through here, so a watch added
// either way behaves identically. msg is the update the watch is made by, which
// an address watch keeps; the Mini App has none and passes nil.
func addWatch(platform string, chat int64, target, alias string, msg []byte) error {
    if app.IsTxID(target) {
        txwatches.Add(target, platform, chat, alias)
    } else {
        if err := watches.Add(platform, chat, target, alias, msg); err != nil { return err }
        startNotifyChat(platform, chat, target, alias)
        if core.Enabled() { seedOutpoints([]string{target}) }
    }
    logging.Info("added subscription %s for %s chat %d (alias %q)", target, platform, chat, alias)
    return nil
}

// removeWatch reverses addWatch and reports whether anything was actually being
// watched, which is what tells a caller "you weren't watching that".
func removeWatch(platform string, chat int64, target string) (bool, error) {
    if app.IsTxID(target) {
        if txwatches.Remove(target, platform, chat) == 0 { return false, nil }
        logging.Info("removed transaction watch %s for %s chat %d", target, platform, chat)
        return true, nil
    }
    var removed, err = watches.Remove(platform, chat, target)
    if err != nil { return false, err }
    if removed == 0 { return false, nil }
    stopNotifyChat(platform, chat, target)
    txwatches.RemoveAddrConfirms(target, platform, chat)
    unwatchScripts(target)
    logging.Info("removed subscription %s for %s chat %d", target, platform, chat)
    return true, nil
}

// setAlias renames an existing watch and reports whether one was there to rename.
// The notifier goroutine holds its alias by value, so an address watch is
// restarted rather than only rewritten — otherwise notifications would keep
// announcing the old name until the next restart. Pending confirmations are
// renamed too, so one already in flight arrives under the new name.
func setAlias(platform string, chat int64, target, alias string) (bool, error) {
    if app.IsTxID(target) { return txwatches.SetAlias(target, platform, chat, alias) > 0, nil }
    var renamed, err = watches.SetAlias(platform, chat, target, alias)
    if err != nil { return false, err }
    if renamed == 0 { return false, nil }
    stopNotifyChat(platform, chat, target)
    startNotifyChat(platform, chat, target, alias)
    txwatches.SetAddrAlias(target, platform, chat, alias)
    logging.Info("renamed subscription %s for chat %d (alias %q)", target, chat, alias)
    return true, nil
}

// watching reports whether this chat already watches target, which is what the
// Mini App's bell renders as pushed or unpushed.
func watching(platform string, chat int64, target string) bool {
    if app.IsTxID(target) {
        for _, e := range txwatches.For(platform, chat) {
            if e.Txid == target { return true }
        }
        return false
    }
    var records, err = watches.List()
    if err != nil {
        logging.Err("list watches: %v", err)
        return false
    }
    for _, r := range records {
        if r.Platform == platform && r.Chat == chat && r.Address == target { return true }
    }
    return false
}

// watchCmd watches what arg names. A bare /watch puts the chat in the
// chats.Watch state, so the next plain message it sends is the argument; one
// with an argument takes the state away. body is the update the watch is made
// by — the one carrying the argument — which an address watch keeps.
func watchCmd(platform string, chat int64, arg string, body []byte) {
    if arg == "" {
        if err := chats.SetState(platform, chat, chats.Watch); err != nil { logging.Err("chat %d state: %v", chat, err) }
        send(platform, chat, i18n(platform, chat).String("Send an address or transaction to watch, optionally followed by an alias — all in one message, e.g. bc1q… John"), nil)
        return
    }
    if _, err := chats.Take(platform, chat, chats.Watch); err != nil { logging.Err("chat %d state: %v", chat, err) }
    var fields = strings.SplitN(arg, " ", 2)
    var watch = fields[0]
    var alias string
    if len(fields) > 1 { alias = strings.TrimSpace(fields[1]) }
    var full, err = atWatchLimit(platform, chat)
    if err != nil {
        logging.Err("count watches: %v", err)
        send(platform, chat, i18n(platform, chat).String("Sorry, something went wrong saving that watch"), nil)
        return
    }
    if full {
        logging.Info("rejected subscription for chat %d: at the limit of %d", chat, maxSubscriptionsPerChat)
        send(platform, chat, i18n(platform, chat).Sprintf("Sorry, this chat has reached the limit of %d subscriptions. Unwatch something first to add a new one.", maxSubscriptionsPerChat), nil)
        return
    }
    if err := addWatch(platform, chat, watch, alias, body); err != nil {
        logging.Err("add watch: %v", err)
        send(platform, chat, i18n(platform, chat).String("Sorry, something went wrong saving that watch"), nil)
        return
    }
    var msg string
    if alias != "" {
        msg = i18n(platform, chat).Sprintf("Watching %s (%s)", html.EscapeString(watch), html.EscapeString(alias))
    } else {
        msg = i18n(platform, chat).Sprintf("Watching %s", html.EscapeString(watch))
    }
    send(platform, chat, msg, nil)
}

// unwatch stops watching what arg names. A bare /unwatch puts the chat in the
// chats.Unwatch state, so the next plain message it sends is the argument; one
// with an argument takes the state away.
func unwatch(platform string, chat int64, arg string) {
    if arg == "" {
        if err := chats.SetState(platform, chat, chats.Unwatch); err != nil { logging.Err("chat %d state: %v", chat, err) }
        send(platform, chat, i18n(platform, chat).String("Send the watch you'd like to stop in a separate message"), nil)
        return
    }
    if _, err := chats.Take(platform, chat, chats.Unwatch); err != nil { logging.Err("chat %d state: %v", chat, err) }
    var removed, err = removeWatch(platform, chat, arg)
    if err != nil {
        logging.Err("remove watch: %v", err)
        send(platform, chat, i18n(platform, chat).String("Sorry, something went wrong removing that watch"), nil)
        return
    }
    if !removed {
        send(platform, chat, i18n(platform, chat).Sprintf("You're not watching %s", html.EscapeString(arg)), nil)
        return
    }
    send(platform, chat, i18n(platform, chat).Sprintf("Stopped watching %s", html.EscapeString(arg)), nil)
}

func watchesCmd(platform string, chat int64) {
    var records, err = watches.List()
    if err != nil {
        logging.Err("list watches: %v", err)
        send(platform, chat, i18n(platform, chat).String("Sorry, something went wrong listing your watches"), nil)
        return
    }
    var addresses, transactions, ids []string
    for _, r := range records {
        if r.Platform != platform || r.Chat != chat { continue }
        ids = append(ids, r.Address)
        var line = "<code>" + html.EscapeString(r.Address) + "</code>"
        if r.Alias != "" { line += " (" + html.EscapeString(r.Alias) + ")" }
        addresses = append(addresses, line)
    }
    for _, e := range txwatches.For(platform, chat) {
        ids = append(ids, e.Txid)
        var line = "<code>" + html.EscapeString(e.Txid) + "</code>"
        if e.Alias != "" { line += " (" + html.EscapeString(e.Alias) + ")" }
        transactions = append(transactions, line)
    }
    if len(addresses) == 0 && len(transactions) == 0 {
        send(platform, chat, i18n(platform, chat).String("You're not watching anything yet"), nil)
        return
    }
    var lines = []string{i18n(platform, chat).String("Your watches:")}
    if len(addresses) > 0 {
        lines = append(lines, "", i18n(platform, chat).String("Addresses:"))
        lines = append(lines, addresses...)
    }
    if len(transactions) > 0 {
        lines = append(lines, "", i18n(platform, chat).String("Transactions:"))
        lines = append(lines, transactions...)
    }
    send(platform, chat, strings.Join(lines, "\n"), ids)
}

const typicalTxVsize = 140

func fees(platform string, chat int64) {
    if !core.Enabled() {
        send(platform, chat, i18n(platform, chat).String("Bitcoin node connection is not configured"), nil)
        return
    }
    feesMu.Lock()
    var rec, ok, count = cachedFees, cachedFeesOK, cachedFeesCount
    feesMu.Unlock()
    if !ok {
        send(platform, chat, i18n(platform, chat).String("Fee estimates aren't available right now — couldn't read the mempool"), nil)
        return
    }
    var tiers = []struct {
        label string
        rate  float64
    }{
        {i18n(platform, chat).String("Fastest (10-20 min):"), rec.fastest},
        {i18n(platform, chat).String("Medium (~1 h):"),       rec.hour},
        {i18n(platform, chat).String("Minimum (2+ h):"),      rec.minimum},
    }
    var pad int
    for _, t := range tiers {
        if n := utf8.RuneCountInString(t.label) + 1; n > pad { pad = n }
    }
    var price, havePrice = rates.Last()
    var lines []string
    for _, t := range tiers {
        var cell = trimNum(t.rate, 2) + i18n(platform, chat).String(" sat/vB")
        if havePrice {
            cell += "  (≈ " + usd(int64(math.Round(t.rate*typicalTxVsize)), price) + ")"
        }
        lines = append(lines, fmt.Sprintf("%-*s %s", pad, t.label, cell))
    }
    var msg = i18n(platform, chat).Sprintf("Network fees projected from %s mempool transactions", group(int64(count)))
    if havePrice {
        msg += ". " + i18n(platform, chat).Sprintf("USD for a typical %d vB transaction", typicalTxVsize)
    }
    send(platform, chat, msg + "\n\n<pre>" + strings.Join(lines, "\n") + "</pre>", nil)
}

// cached fees recommendation, recomputed every 10 minutes alongside
// startMempoolFees so /fees can reply instantly and confEstimate can
// pick up the current fee landscape without an RPC call.
var feesMu sync.Mutex
var cachedFees recommendedFees
var cachedFeesOK bool
var cachedFeesCount int // number of mempool entries the fees were projected from

// startMempoolSummary recomputes the mempool totals (output amount + fees) every
// 10 minutes in the background and stores them so /mempool can reply instantly
// with a ~-prefixed cached value instead of summing on every request. Each
// recompute replaces the amount the rawtx frames have moved, and tells open
// mempool pages.
func startMempoolSummary() {
    if !core.Enabled() { return }
    go func() {
        var calc = func() {
            var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
            defer cancel()
            mp.SetSummary(mempoolTotals(ctx))
            app.Notify("mempool")
        }
        calc()
        var t = time.NewTicker(10 * time.Minute)
        defer t.Stop()
        for range t.C {
            calc()
        }
    }()
}

// startMempoolFees recomputes the fee recommendations (projected blocks +
// recommended fees) every 10 minutes and stores them so /fees can reply
// instantly and confEstimate can use current mempool-based rates instead of
// core's slow-to-react estimatesmartfee.
// activeNodeWindow is how recently the node must have seen a peer for it to
// count as active. A var so tests can shrink it.
var activeNodeWindow = 48 * time.Hour

var networkMu sync.Mutex
var cachedNetwork app.Network

// btcnodesURL is BTC Nodes' list of reachable nodes. One page of one row is
// enough: the response carries the total in its count field. A var so tests
// point it at a local server.
var btcnodesURL = "https://btcnodes.io/api/v1/nodes/?limit=1"

// btcnodesClient fetches the count with its own timeout, like the rate and
// market clients, so a slow API cannot stall a refresh.
var btcnodesClient = &http.Client{Timeout: 10 * time.Second}

// btcnodesTTL is how long a successful btcnodes.io count is reused before the
// API is asked again. Six hours is four requests a day, inside its daily limit;
// the same interval also spaces out retries after a failure, so a down or
// throttled API is not hammered between ticks.
var btcnodesTTL = 6 * time.Hour

// btcnodesMaxAge is how old the cached count may get before the refresh stops
// trusting it. More than two days old and the node's own getnodeaddresses scan
// takes over.
var btcnodesMaxAge = 48 * time.Hour

// The btcnodes.io crawl, cached. Count is the last successful count (0 means
// none yet); seen is when that count was fetched; next is the earliest time the
// API may be asked again. All are touched only by the network-stats ticker, so
// they need no lock.
var btcnodesCount int64
var btcnodesSeen time.Time
var btcnodesNext time.Time

// startNetworkStats keeps the Mini App's Network card fresh. getnodeaddresses
// returns every address the node knows — 65k entries and several megabytes on
// mainnet — so this must never run in a request path.
// startMarketUpdates pushes the Market card on a ticker. There is nothing to
// recompute in the background — the card reads the rate history per request —
// so this only sends the signal that tells open pages to re-fetch.
func startMarketUpdates() {
    go func() {
        var t = time.NewTicker(10 * time.Minute)
        defer t.Stop()
        for range t.C {
            app.Notify("market")
        }
    }()
}

// which woke it decides whether the peer count is re-scanned: a
// block moves the height and nothing else here, and the scan is the
// one expensive call (see refreshNetwork).
func startNetworkStats() {
    if !core.Enabled() { return }
    go func() {
        var wake = signals.Subscribe(signals.Block)
        var funded = signals.Subscribe(signals.AddrBal)
        refreshNetwork(true)
        var t = time.NewTicker(10 * time.Minute)
        defer t.Stop()
        for {
            select {
            case <-t.C:
                refreshNetwork(true)
            case <-wake:
                refreshNetwork(false)
            case <-funded:
                refreshNetwork(false)
            }
        }
    }()
}

// btcnodesActive fetches the count of reachable Bitcoin nodes from btcnodes.io.
// That crawl probes every known node and counts the ones that answer, so it is
// the network-wide "active nodes" figure — smaller than the node's own address
// manager, which counts every gossiped address whether or not it is reachable.
// It reports an error when the API is unreachable, throttled (429), or the
// response cannot be parsed.
func btcnodesActive(ctx context.Context) (int64, error) {
    logging.Net("network: GET %s", btcnodesURL)
    var req, err = http.NewRequestWithContext(ctx, http.MethodGet, btcnodesURL, nil)
    if err != nil { return 0, err }
    var resp, derr = btcnodesClient.Do(req)
    if derr != nil { return 0, derr }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return 0, fmt.Errorf("status %d", resp.StatusCode)
    }
    var v struct {
        Count int64 `json:"count"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&v); err != nil { return 0, err }
    if v.Count <= 0 { return 0, fmt.Errorf("non-positive count %d", v.Count) }
    return v.Count, nil
}

// resolveActiveNodes returns the "Active nodes" figure for the Network card.
// The btcnodes.io crawl is the preferred source — it counts reachable, verified
// nodes rather than gossip. A successful result is reused for btcnodesTTL (six
// hours — four requests a day, inside the API's daily limit), and remains
// authoritative for up to btcnodesMaxAge (two days). Only when the cached count
// is older than that, or was never fetched, does the node's own getnodeaddresses
// scan take over; and if that too fails, prev — the last known figure — is
// carried forward rather than showing a misleading 0.
func resolveActiveNodes(ctx context.Context, prev string) string {
    if time.Now().Before(btcnodesNext) {
        return btcnodesOrScan(ctx, prev)
    }
    btcnodesNext = time.Now().Add(btcnodesTTL)
    var count, err = btcnodesActive(ctx)
    if err == nil {
        btcnodesCount, btcnodesSeen = count, time.Now()
        return group(count)
    }
    logging.Warn("network stats: btcnodes.io: %v", err)
    return btcnodesOrScan(ctx, prev)
}

// btcnodesOrScan serves the cached btcnodes count while it is young enough to
// trust, and falls back to the node's own scan once it is not.
func btcnodesOrScan(ctx context.Context, prev string) string {
    if btcnodesCount > 0 && time.Since(btcnodesSeen) <= btcnodesMaxAge {
        return group(btcnodesCount)
    }
    var addrs, aerr = core.GetNodeAddresses(ctx)
    if aerr == nil {
        var cutoff = time.Now().Add(-activeNodeWindow).Unix()
        var active int64
        for _, a := range addrs {
            if a.Time >= cutoff { active++ }
        }
        return group(active)
    }
    logging.Warn("network stats: node addresses: %v", aerr)
    return prev
}

// refreshNetwork rebuilds the Blockchain card and pushes it to any open page.
//
// withNodes gates the expensive calls behind the ticker. The "Active nodes"
// figure prefers btcnodes.io's reachable-node crawl (see resolveActiveNodes),
// and a new block asks for none of it, since the count barely moves between
// blocks.
//
// A failed peer count must not blank the field or, worse, report 0 active
// nodes — keep whatever we last knew.
//
// Addresses is how many hold a positive balance, which the addrbal scan
// counts; until it has caught up with the chain the figure is only part of it,
// so the field says nothing rather than something too small.
func refreshNetwork(withNodes bool) {
    if !core.Enabled() { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    var info, err = core.GetBlockchainInfo(ctx)
    if err != nil {
        logging.Warn("network stats: %v", err)
        return
    }
    networkMu.Lock()
    var nodes, txs = cachedNetwork.Nodes, cachedNetwork.Txs
    networkMu.Unlock()
    if stats, serr := core.GetChainTxStats(ctx); serr == nil {
        txs = bigCount(stats.TxCount)
    } else {
        logging.Warn("network stats: chain tx stats: %v", serr)
        if txs == "" { txs = "—" }
    }
    if withNodes {
        nodes = resolveActiveNodes(ctx, nodes)
    }
    if nodes == "" { nodes = "—" }
    var funded = "—"
    if n, ok := addrbal.Count(); ok { funded = bigCount(n) }
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true,
        Coins:  metric(toBTC(blocks.CirculatingSupply(info.Blocks)), 1),
        Cap:    "21 M",
        Blocks: group(info.Blocks),
        Size:   humSize(info.SizeOnDisk, 0, ""),
        Nodes:  nodes,
        Txs:    txs,
        Addresses: funded,
    }
    networkMu.Unlock()
    app.Notify("network")
}

func startMempoolFees() {
    if !core.Enabled() { return }
    go func() {
        var wake = signals.Subscribe(signals.Block)
        refreshFees()
        var t = time.NewTicker(10 * time.Minute)
        defer t.Stop()
        for {
            select {
            case <-t.C:
            case <-wake:
            }
            refreshFees()
        }
    }()
}

// refreshFees recomputes the projection and pushes it to any open page.
func refreshFees() {
    if !core.Enabled() { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    var entries, err = core.RawMempoolVerbose(ctx)
    if err != nil {
        logging.Warn("fees: %v", err)
        return
    }
    var minFee float64
    if info, ierr := core.GetMempoolInfo(ctx); ierr == nil {
        minFee = info.MempoolMinFee
    }
    var rec = calculateRecommendedFee(buildProjectedBlocks(entries), minFee)
    feesMu.Lock()
    cachedFees, cachedFeesOK, cachedFeesCount = rec, true, len(entries)
    feesMu.Unlock()
    app.Notify("fees")
}

// mempoolSummaryLimit caps how many transactions /mempool will total up — above
// it, the totals (which need the whole verbose mempool plus a fetch per tx) would
// take too long, so the reply degrades to just size and count. A package var so
// it's tunable/testable.
var mempoolSummaryLimit int64 = 20000

// mempoolCmd replies with the current mempool size and transaction count, plus —
// when the mempool is small enough to total up in reasonable time — the summed
// output amount and summed fees of every mempool transaction, in sats and USD.
func mempoolCmd(platform string, chat int64) {
    if !core.Enabled() {
        send(platform, chat, i18n(platform, chat).String("Bitcoin node connection is not configured"), nil)
        return
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    var info, err = core.GetMempoolInfo(ctx)
    if err != nil {
        logging.Err("get mempool info: %v", err)
        send(platform, chat, i18n(platform, chat).String("Sorry, something went wrong reading the mempool"), nil)
        return
    }
    var pairs = [][2]string{
        {i18n(platform, chat).String("Size"),         humSize(info.Bytes, 2, chatLang(platform, chat))},
        {i18n(platform, chat).String("Transactions"), group(int64(info.Size))},
    }
    var rate, rateOK, change, changeOK = mp.Flow(time.Now())
    if rateOK {
        var fr = i18n(platform, chat).Sprintf("%.1f tx/sec", rate)
        if changeOK {
            fr += fmt.Sprintf(" (%+.1f)", change)
        }
        pairs = append(pairs, [2]string{i18n(platform, chat).String("Flow rate"), fr})
    }
    if info.Size > 0 {
        var amount, fee, ok = mp.Summary()
        if ok {
            pairs = append(pairs,
                [2]string{i18n(platform, chat).String("Total flow"), "~" + btcAmount(amount)},
                [2]string{i18n(platform, chat).String("Total fees"), "~" + btcAmount(fee)},
            )
        }
    }
    send(platform, chat, i18n(platform, chat).Sprintf("Mempool\n\n<pre>%s</pre>", joinAlign(pairs)), nil)
}

// mempoolTotals sums the fee (from the verbose mempool) and the output amount
// (fetching each transaction, concurrently and bounded) across the whole
// mempool. Individual tx churn — a tx confirmed or replaced between the two
// passes — is tolerated (its outputs are just skipped); a context timeout means
// the mempool was too large and returns ok=false so the reply shows "unavailable".
func mempoolTotals(ctx context.Context) (amount, fee int64, ok bool) {
    var pool, err = core.RawMempoolVerbose(ctx)
    if err != nil { return 0, 0, false }
    var txids = make([]string, 0, len(pool))
    for id, e := range pool {
        fee += toSat(e.Fees.Base)
        txids = append(txids, id)
    }
    var mu sync.Mutex
    var wg sync.WaitGroup
    var sem = make(chan struct{}, 16)
    for _, id := range txids {
        wg.Add(1)
        sem <- struct{}{}
        go func(id string) {
            defer wg.Done()
            defer func() { <-sem }()
            var tx, e = core.GetRawTransaction(ctx, id)
            if e != nil { return }
            var out int64
            for _, v := range tx.Vout { out += toSat(v.Value) }
            mu.Lock()
            amount += out
            mu.Unlock()
        }(id)
    }
    wg.Wait()
    if ctx.Err() != nil { return 0, 0, false }
    return amount, fee, true
}

// minersCmd replies with the top 10 mining pools by blocks mined over the window
// the stats collector has processed, each with its total reward, fees, and an
// estimated power draw (see the miners package).
func minersCmd(platform string, chat int64) {
    var top = miners.Top(10)
    if len(top) == 0 {
        send(platform, chat, i18n(platform, chat).String("No miner statistics yet — still collecting"), nil)
        return
    }
    var lines []string
    for i, m := range top {
        var blocks = i18np(chatLang(platform, chat)).Plural(int64(m.Blocks), "block")
        lines = append(lines, i18n(platform, chat).Sprintf("%d. %s. %s mined, reward %s BTC, fees %s BTC, consumption %s GW",
            i+1, m.Name, blocks, trimNum(toBTC(m.Reward), 2), trimNum(toBTC(m.Fees), 2), trimNum(m.ConsumptionGW, 1)))
    }
    send(platform, chat, i18n(platform, chat).String("Top miners by blocks mined:") + "\n\n" + strings.Join(lines, "\n"), nil)
}

// marketCmd reports the current price, market capitalisation and 24h volume,
// plus how the price has moved over several periods.
//
// Everything is read from the database: the price and its history from the rate
// series, capitalisation and volume from the market snapshot the rates updater
// stores on its 5-minute tick. Nothing here calls a third-party API, so the
// command answers at storage speed and an API that is down or rate-limiting
// costs a slightly stale figure rather than a failed reply.
//
// The changes are computed from the bot's own history — a daily series back to
// 2009 plus live 5-minute samples — so any period can be answered without
// another dependency, and the figures stay consistent with the USD values shown
// elsewhere.
func marketCmd(platform string, chat int64) {
    var now, haveNow = rates.Last()
    var snapshot, haveSnapshot = rates.LastMarket()
    if haveSnapshot && snapshot.Price > 0 {
        now, haveNow = snapshot.Price, true
    }
    if !haveNow {
        send(platform, chat, i18n(platform, chat).String("No price data yet — still fetching"), nil)
        return
    }
    var pairs = [][2]string{{i18n(platform, chat).String("Price"), rates.Price(now)}}
    if haveSnapshot && snapshot.MarketCap > 0 {
        pairs = append(pairs, [2]string{i18n(platform, chat).String("Market cap"), money(snapshot.MarketCap, chatLang(platform, chat))})
    } else {
        pairs = append(pairs, [2]string{i18n(platform, chat).String("Market cap"), i18n(platform, chat).String("unavailable")})
    }
    if haveSnapshot && snapshot.Volume24h > 0 {
        pairs = append(pairs, [2]string{i18n(platform, chat).String("Volume 24h"), money(snapshot.Volume24h, chatLang(platform, chat))})
    } else {
        pairs = append(pairs, [2]string{i18n(platform, chat).String("Volume 24h"), i18n(platform, chat).String("unavailable")})
    }
    var periods = []struct {
        label string
        back  time.Duration
    }{
        {i18n(platform, chat).String("1 day"),    24 * time.Hour},
        {i18n(platform, chat).String("1 week"),   7 * 24 * time.Hour},
        {i18n(platform, chat).String("1 month"),  30 * 24 * time.Hour},
        {i18n(platform, chat).String("3 months"), 90 * 24 * time.Hour},
        {i18n(platform, chat).String("1 year"),   365 * 24 * time.Hour},
        {i18n(platform, chat).String("5 years"),  5 * 365 * 24 * time.Hour},
    }
    var changes [][2]string
    for _, p := range periods {
        if then, ok := rates.At(time.Now().Add(-p.back)); ok && then > 0 {
            changes = append(changes, [2]string{p.label, rates.Change(now, then)})
        }
    }
    if len(changes) > 0 {
        pairs = append(pairs, [2]string{i18n(platform, chat).String("Changes"), ""})
        for _, c := range changes { pairs = append(pairs, c) }
    }
    send(platform, chat, i18n(platform, chat).Sprintf("Bitcoin market\n\n<pre>%s</pre>", joinAlign(pairs)), nil)
}

// send replies in a chat on its platform, with a button for each id the text
// shows, and logs it. Which ids get a button is decided here, so a message
// carries the same keyboard on either platform.
func send(platform string, chat int64, text string, ids []string) {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    var buttons = buttonIDs(ids)
    var err error
    switch platform {
    case telegram.Platform:
        err = telegram.Send(ctx, chat, text, buttons)
    case max.Platform:
        err = max.Send(ctx, chat, text, buttons)
    default:
        err = fmt.Errorf("unknown platform %q", platform)
    }
    if err != nil {
        logging.Err("send message: %v", err)
        return
    }
    logSent(platform, chat, text)
}

// maxButtons bounds how many buttons one message carries, so an /info on a
// transaction with many addresses doesn't bury the message under a keyboard.
const maxButtons = 8

// buttonIDs picks the ids that get a button, in order: duplicates, empties, the
// "(non-standard)" placeholder outputs use, and anything over Telegram's
// 64-byte callback_data are dropped, and at most maxButtons are kept.
func buttonIDs(ids []string) []string {
    var seen = map[string]bool{}
    var out []string
    for _, id := range ids {
        if id == "" || id == "(non-standard)" || seen[id] || len(id) > 64 { continue }
        seen[id] = true
        out = append(out, id)
        if len(out) == maxButtons { break }
    }
    return out
}

// minerChart backs the chart under the Mini App's miner page: what one pool mined in each day of the last 30, each week of the
// last 13 or each month of the last 12, read out of the blocks table. The buckets
// are calendar ones in UTC, so the last is the one still running; a week starts
// on a Monday.
//
// Consumption needs every block in a bucket, not only the pool's: it is the pool's
// share of the bucket's blocks at the difficulty its own blocks were mined at —
// the same formula /miners applies to the whole history, through
// miners.Consumption. The share is of all the blocks, unattributed ones included,
// since those were mined by somebody.
//
// OK is false when the table holds no block in the period at all, which is a
// collector that has not got there yet rather than a pool that mined nothing.
//
// where a month follows a day, and where it stands alone — Russian declines
// the two differently ("14 мая" against "май 2026")
//
// blocks_ts is what makes this a range of the index rather than a scan of
// every block on the chain
//
// a block stamped ahead of now lands in the last bucket, which is where
// the search leaves anything past the last start
//
// the first, the last and the one in the middle: enough to read the
// period off, and few enough not to collide at a phone's width
func minerChart(lang, name, data, period string, now time.Time) app.Chart {
    var out = app.Chart{Name: name, Data: data, Period: period}
    if db == nil { return out }
    var t = i18nl(lang)
    var short = []string{t.String("jan"), t.String("feb"), t.String("mar"), t.String("apr"), t.String("may"),
        t.String("jun"), t.String("jul"), t.String("aug"), t.String("sep"), t.String("oct"), t.String("nov"), t.String("dec")}
    var alone = []string{t.String("jan-alone"), t.String("feb-alone"), t.String("mar-alone"), t.String("apr-alone"),
        t.String("may-alone"), t.String("jun-alone"), t.String("jul-alone"), t.String("aug-alone"), t.String("sep-alone"),
        t.String("oct-alone"), t.String("nov-alone"), t.String("dec-alone")}
    now = now.UTC()
    var today = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
    var starts []time.Time
    switch period {
    case "quarter":
        var monday = today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
        for i := 12; i >= 0; i-- { starts = append(starts, monday.AddDate(0, 0, -7*i)) }
        out.Label = t.String("last 13 weeks")
    case "year":
        var first = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
        for i := 11; i >= 0; i-- { starts = append(starts, first.AddDate(0, -i, 0)) }
        out.Label = t.String("last 12 months")
    default:
        for i := 29; i >= 0; i-- { starts = append(starts, today.AddDate(0, 0, -i)) }
        out.Label = t.String("last 30 days")
    }
    var mined, all = make([]int64, len(starts)), make([]int64, len(starts))
    var difficulty = make([]float64, len(starts))
    var rows, err = db.Query("select ts, miner, difficulty from blocks where ts >= ?", starts[0].Unix())
    if err != nil {
        logging.Warn("mini app: miner chart: %v", err)
        return out
    }
    defer rows.Close()
    for rows.Next() {
        var ts int64
        var miner string
        var d float64
        if rows.Scan(&ts, &miner, &d) != nil { continue }
        var i = sort.Search(len(starts), func(i int) bool { return starts[i].Unix() > ts }) - 1
        all[i]++
        if miner == name {
            mined[i]++
            difficulty[i] += d
        }
    }
    if err := rows.Err(); err != nil {
        logging.Warn("mini app: miner chart: %v", err)
        return out
    }
    var consumption = func(m, n int64, d float64) float64 {
        if m == 0 { return 0 }
        return miners.Consumption(float64(m)/float64(n), d/float64(m))
    }
    var format = func(v float64) string {
        if data == "consumption" { return trimNum(v, 2) + " GW" }
        return i18np(lang).Plural(int64(v), "block")
    }
    var values = make([]float64, len(starts))
    var total, blocks, peak = int64(0), int64(0), 0.0
    var totalDifficulty float64
    for i := range starts {
        values[i] = float64(mined[i])
        if data == "consumption" { values[i] = consumption(mined[i], all[i], difficulty[i]) }
        if values[i] > peak { peak = values[i] }
        total += mined[i]
        blocks += all[i]
        totalDifficulty += difficulty[i]
    }
    if blocks == 0 { return out }
    out.OK = true
    out.Value = format(float64(total))
    if data == "consumption" { out.Value = format(consumption(total, blocks, totalDifficulty)) }
    var top = niceTop(peak, data != "consumption")
    out.Top, out.Mid = strconv.FormatFloat(top, 'f', -1, 64), strconv.FormatFloat(top/2, 'f', -1, 64)
    for i, s := range starts {
        var day = strconv.Itoa(s.Day()) + " " + short[s.Month()-1]
        var bar = app.Bar{Height: math.Round(values[i]/top*1000) / 10, Value: format(values[i]),
            Label: day + " " + strconv.Itoa(s.Year())}
        var tick = day
        switch period {
        case "quarter":
            var end = s.AddDate(0, 0, 6)
            var from = day
            if s.Year() != end.Year() { from += " " + strconv.Itoa(s.Year()) }
            bar.Label = from + " – " + strconv.Itoa(end.Day()) + " " + short[end.Month()-1] + " " + strconv.Itoa(end.Year())
        case "year":
            bar.Label = alone[s.Month()-1] + " " + strconv.Itoa(s.Year())
            tick = bar.Label
        }
        if i == 0 || i == len(starts)/2 || i == len(starts)-1 { bar.Tick = tick }
        out.Bars = append(out.Bars, bar)
    }
    return out
}

// niceTop is the top of a chart's scale: the smallest of 1, 2, 4, 6 or 8 times a
// power of ten that holds v, so the scale and its half both read as round
// numbers. A count's scale is at least 2, or its half would be half a block.
//
// A negative power is divided by rather than multiplied, because 10^-1 has no
// exact float and 6 × it prints as 0.6000000000000001; 6 / 10 is the float
// nearest 0.6, and so is what strconv prints back as "0.6".
func niceTop(v float64, whole bool) float64 {
    if whole && v < 2 { return 2 }
    if v <= 0 { return 1 }
    for k := int(math.Floor(math.Log10(v))); ; k++ {
        for _, m := range []float64{1, 2, 4, 6, 8} {
            var top = m * math.Pow10(k)
            if k < 0 { top = m / math.Pow10(-k) }
            if top >= v { return top }
        }
    }
}
