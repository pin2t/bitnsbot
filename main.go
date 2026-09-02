package main

import "context"
import "crypto/subtle"
import "encoding/hex"
import "encoding/json"
import "flag"
import "fmt"
import "html"
import "net/http"
import "os"
import "os/signal"
import "strings"
import "sync"
import "syscall"
import "time"
import "runtime/debug"
import "go.etcd.io/bbolt"
import "github.com/pin2t/flagex"
import "bitnsbot/app"
import "bitnsbot/dbui"
import "bitnsbot/logging"
import "bitnsbot/miners"
import "bitnsbot/rates"
import "bitnsbot/txwatches"
import "bitnsbot/watches"
import "unicode/utf8"
import "sort"
import "math"

var configPath      = flag.String("config", "", "path to a properties file (name=value lines) with flag values; command-line flags take precedence")
var verbose         = flag.Int("verbose", 0, "log verbosity: 0=ERR/WARN/status, 1=+INFO, 2=+NET/DB (raw external traffic and storage requests)")
var botToken        = flag.String("bot-token", "", "Telegram bot token authenticating outbound Bot API calls (required)")
var listenAddr      = flag.String("listen", ":8082", "address the Telegram webhook server binds to")
var webhookPath     = flag.String("webhook-path", "/bot", "path the Bot API server will POST updates to")
var webhookURL      = flag.String("webhook-url", "", "URL the Bot API server should send updates to, e.g. http://localhost:8080/bot")
var apiBaseURL      = flag.String("api-base-url", "http://localhost:8081", "base URL of the local telegram-bot-api server")
var secretToken     = flag.String("secret-token", "", "optional secret checked against the X-Telegram-Bot-Api-Secret-Token header")
var registerHook    = flag.Bool("register-webhook", true, "call setWebhook on startup")
var dbPath          = flag.String("db", "watches.db", "path to the bbolt watches database")
var coreURL         = flag.String("core-url", "", "Bitcoin Core JSON-RPC URL, e.g. http://127.0.0.1:8332 (leave empty to skip connecting to the node)")
var coreUser        = flag.String("core-user", "", "Bitcoin Core RPC username (or use -core-cookie)")
var corePass        = flag.String("core-pass", "", "Bitcoin Core RPC password (or use -core-cookie)")
var coreCookie      = flag.String("core-cookie", "", "path to Bitcoin Core's .cookie file, an alternative to -core-user/-core-pass")
var coreZMQ         = flag.String("core-zmq", "", "comma-separated ZMQ endpoints publishing hashblock and rawtx, e.g. tcp://127.0.0.1:28332,tcp://127.0.0.1:28333")
var coreREST        = flag.String("core-rest", "", "base URL of Bitcoin Core's REST interface for building the address index (empty disables indexing)")
var backupPath      = flag.String("backup", "", "path to copy the database to periodically (empty disables backups)")
var backupInterval  = flag.Duration("backup-interval", 24*time.Hour, "how often to back up the database")
var backupScript    = flag.String("backup-script", "", "command run after each backup, with the backup's path as $1 and in $BACKUP_FILE (empty runs nothing)")
var logNoTs         = flag.Bool("log-no-ts", false, "omit the date and time prefix from each log line")
var appListen       = flag.String("app-listen", "127.0.0.1:8080", "address the Telegram Mini App web server binds to (empty disables it; bind to localhost — the Cloudflare tunnel is what faces the network)")
var dbuiListen      = flag.String("dbui-listen", "", "address for the database admin web UI, e.g. 127.0.0.1:8090 (empty disables it; bind to localhost only — it can write any bucket)")
var historyFile     = flag.String("history-file", "", "path to a JSON file containing historical BTC/USD rates (same format as blockchain.info/charts/market-price); backfilled from this file on first run instead of fetching over the network")

var core *coreConn
var dbuiSrv *http.Server
var appSrv *http.Server

// appSource adapts main's fee cache and formatters to app.Source, so the app
// package stays unaware of Bitcoin Core, the price feeds and the cache.
// It carries the bot because adding a watch starts a notifier goroutine, which
// needs something to send the notification with.
type appSource struct{ bot *bot }

func (appSource) Network() app.Network {
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

// Blocks reads one window of the recent-block list straight out of the
// blocks-stat bucket. The keys are big-endian heights, so walking the cursor
// backwards yields newest-first order with no sorting and no node round trip,
// and Seek starts a batch at a given height without stepping over the ones above
// it — everything the row needs is already in the cached record.
func (appSource) Blocks(rng app.Range) app.Blocks {
    var out = app.Blocks{Top: rng.After}
    if db == nil { return out }
    var limit = blocksPerPage
    if rng.Down > 0 { limit = blocksMaxRows }
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(blocksBucket)
        if b == nil { return nil }
        var c = b.Cursor()
        var k, v = c.Last()
        if rng.Before > 0 {
            // Seek lands on the first key at or above Before, so one step back
            // is the block below it; past the tip it returns nothing, which is
            // the whole list.
            if k, v = c.Seek(itob(uint64(rng.Before))); k == nil {
                k, v = c.Last()
            } else {
                k, v = c.Prev()
            }
        }
        for ; k != nil; k, v = c.Prev() {
            var bi blockInfo
            // A record written before a schema change fails to decode; skip it
            // rather than letting one bad row end the batch early.
            if json.Unmarshal(v, &bi) != nil { continue }
            if rng.After > 0 && bi.Height <= rng.After { break }
            if len(out.Rows) == limit {
                out.More = true
                break
            }
            var miner = bi.Miner
            if miner == "" { miner = "Unknown" }
            out.Rows = append(out.Rows, app.Block{
                Height:     group(bi.Height),
                Num:        bi.Height,
                Size:       humSize(int64(bi.Size), 2, 0),
                Txs:        group(int64(bi.NumTx)) + " txs",
                Miner:      miner,
                MinerKnown: bi.Miner != "",
            })
            if rng.Down > 0 && bi.Height <= rng.Down {
                var next, _ = c.Prev()
                out.More = next != nil
                break
            }
        }
        return nil
    })
    if len(out.Rows) > 0 {
        out.Top, out.Next = out.Rows[0].Num, out.Rows[len(out.Rows)-1].Num
    }
    out.OK = len(out.Rows) > 0
    return out
}

// BlockInfo backs the Mini App's block details page. It loads or computes the
// same record /info uses and renders the same lines from it, in English —
// chat 0 has no language, so i18n falls through to the source strings.
func (appSource) BlockInfo(height int64) app.Info {
    // Title is set even when the lookup fails, so the page still names what was
    // asked for rather than reading "Block ".
    var out = app.Info{Title: "Block " + group(height)}
    var bi, ok = loadBlock(height)
    if !ok {
        if core == nil { return out }
        var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
        defer cancel()
        var hash, err = core.getBlockHash(ctx, height)
        if err != nil { return out }
        bi, err = computeBlockInfo(ctx, hash)
        if err != nil {
            logging.Warn("mini app: compute block %d: %v", height, err)
            return out
        }
        storeBlock(bi)
    }
    out.OK = true
    out.Rows = appFields(blockPairs(bi, 0))
    return out
}

// TxInfo backs the transaction details page. A block hash has the same 64-hex
// shape as a txid and only the node can tell them apart, so — exactly as info()
// does for the bot — an id the node has a block header for is shown as that
// block instead. Both live on the Blocks tab, so the handoff is seamless.
func (appSource) TxInfo(txid string) app.Info {
    var out = app.Info{Title: short(txid)}
    if core == nil { return out }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    if header, err := core.getBlockHeader(ctx, txid); err == nil {
        return appSource{}.BlockInfo(header.Height)
    }
    var pairs, _, canonical, ok = txPairs(ctx, 0, txid)
    if !ok { return out }
    return app.Info{OK: true, Title: short(canonical), Rows: appFields(pairs)}
}

// AddrInfo backs the address details page. An input that is not an address at
// all says so, rather than reporting an empty history as fact.
func (appSource) AddrInfo(addr string) app.Info {
    var out = app.Info{Title: short(addr)}
    if core == nil { return out }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    var pairs, valid, err = addrPairs(ctx, 0, addr)
    if err != nil {
        logging.Warn("mini app: address %s: %v", short(addr), err)
        return out
    }
    if !valid {
        out.Rows = []app.Field{{Label: "this does not look like a Bitcoin address"}}
        return out
    }
    out.OK, out.Rows = true, appFields(pairs)
    return out
}

// Watching and SetWatch back the Mini App's watch button. They go through the
// same addWatch/removeWatch the /watch and /unwatch commands use, so a watch
// added from the app fires notifications exactly like one added from the chat.
func (appSource) Watching(chat int64, kind, id string) bool { return watching(chat, id) }

// SetWatch adds or removes a watch and reports the state it ended in — which is
// what the button renders, so a refusal shows as unpushed rather than lying.
func (s appSource) SetWatch(chat int64, kind, id string, on bool) (bool, error) {
    if !on {
        var _, err = removeWatch(chat, id)
        return false, err
    }
    if watching(chat, id) { return true, nil }
    var full, err = atWatchLimit(chat)
    if err != nil { return false, err }
    if full {
        logging.Info("mini app: rejected watch for chat %d: at the limit of %d", chat, maxSubscriptionsPerChat)
        return false, nil
    }
    if err := addWatch(s.bot, chat, id, ""); err != nil { return false, err }
    return true, nil
}

// MinerInfo backs the miner details page, opened from a pool name in the block
// list. The figures and their formatting are /miners', so the two agree.
func (appSource) MinerInfo(name string) app.Info {
    var out = app.Info{Title: name}
    var s, ok = miners.Get(name)
    if !ok { return out }
    var blocks = i18n(0).Sprintf("%d blocks", s.Blocks)
    if s.Blocks == 1 { blocks = i18n(0).String("1 block") }
    out.OK = true
    out.Rows = []app.Field{
        {Label: "Blocks mined", Value: blocks},
        {Label: "Reward", Value: trimNum(toBTC(s.Reward), 2) + " BTC"},
        {Label: "Fees", Value: trimNum(toBTC(s.Fees), 2) + " BTC"},
        {Label: "Consumption", Value: trimNum(s.ConsumptionGW, 1) + " GW"},
    }
    return out
}

// Watches backs the Watches tab: the calling user's own watches, and nobody
// else's. chat comes from the signed initData, and the same chat-scoping the bot
// applies in watchesCmd is what keeps one user's list out of another's.
func (appSource) Watches(chat int64) app.Watches {
    var records, err = watches.List()
    if err != nil {
        logging.Err("mini app: list watches: %v", err)
        return app.Watches{}
    }
    var out = app.Watches{OK: true}
    for _, r := range records {
        if r.Chat != chat { continue }
        out.Addresses = append(out.Addresses, app.Watch{Short: short(r.Address), Id: r.Address, Alias: r.Alias})
    }
    for _, e := range txwatches.For(chat) {
        out.Txs = append(out.Txs, app.Watch{Short: short(e.Txid), Id: e.Txid, Alias: e.Alias})
    }
    return out
}

// appFields turns the bot's label/value pairs into the app's rows. English only:
// chat 0 has no language, so i18n falls through to the source strings.
func appFields(pairs [][2]string) []app.Field {
    var out = make([]app.Field, 0, len(pairs))
    for _, p := range pairs { out = append(out, app.Field{Label: p[0], Value: p[1]}) }
    return out
}

// Market reads the rate history straight from the database — cheap enough that
// the card needs no cache of its own, unlike fees and the chain stats.
func (appSource) Market() app.Market {
    var now, ok = rates.Last()
    if !ok { return app.Market{} }
    var m = app.Market{OK: true, Price: price(now)}
    var periods = []struct {
        label string
        back  time.Duration
    }{
        {"1d", 24 * time.Hour},
        {"1w", 7 * 24 * time.Hour},
        {"1mo", 30 * 24 * time.Hour},
        {"3mo", 90 * 24 * time.Hour},
        {"1y", 365 * 24 * time.Hour},
        {"5y", 5 * 365 * 24 * time.Hour},
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

func (appSource) Fees() app.Fees {
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

var ver = "1.2"
var commit = ""

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
    if *configPath != "" {
        if err := flagex.ParseFile(*configPath); err != nil {
            logging.Fatal("apply config: %v", err)
        }
    }
    logging.SetVerbose(*verbose)
    if *logNoTs {
        logging.DisableTimestamp()
    }
    if *botToken == "" {
        logging.Fatal("-bot-token is required")
    }
    var bot = newBot(*botToken, *apiBaseURL)
    var err error
    if err = openDB(*dbPath); err != nil {
        logging.Fatal("open database: %v", err)
    }
    rates.SetHistoryFile(*historyFile)
    rates.Start()
    if *dbuiListen != "" {
        dbuiSrv = dbui.Start(db, *dbuiListen)
    }
    if *appListen != "" {
        appSrv = app.Start(*appListen, *botToken, appSource{bot: bot})
    }
    if *backupPath != "" {
        startBackup(*backupPath, *backupInterval, *backupScript)
        logging.Status("backing up the database to %s every %s", *backupPath, *backupInterval)
    }
    if *coreURL != "" {
        core, err = newCoreConn(*coreURL, *coreUser, *corePass, *coreCookie)
        if err != nil {
            logging.Fatal("Bitcoin Core client: %v", err)
        }
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var height, herr = core.getBlockCount(ctx)
        cancel()
        if herr != nil {
            logging.Fatal("Bitcoin Core at %s: %v", *coreURL, herr)
        }
        logging.Status("connected to Bitcoin Core at %s (height %d)", *coreURL, height)
    }
    startNotify(bot)
    miners.Start()
    if core != nil {
        miners.StartStats(minerSource{})
    }
    startBlockCache()
    startMempoolFlow()
    startMempoolSummary()
    startMempoolFees()
    startNetworkStats()
    startMarketUpdates()
    if core != nil && *coreZMQ != "" {
        if err := startZMQ(context.Background(), strings.Split(*coreZMQ, ","), bot); err != nil {
            logging.Fatal("subscribe to Bitcoin Core ZMQ: %v", err)
        }
    }
    if core != nil && *coreREST != "" {
        startAddrIndex(*coreREST)
    }
    if *registerHook {
        if *webhookURL == "" {
            logging.Fatal("-webhook-url is required when -register-webhook=true")
        }
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var err = bot.setWebhook(ctx, *webhookURL, *secretToken)
        cancel()
        if err != nil {
            logging.Fatal("set webhook: %v", err)
        }
        logging.Status("webhook registered at %s", *webhookURL)
        registerMenu(bot)
    }
    http.HandleFunc(*webhookPath, webhookHandler(bot))
    var srv = &http.Server{Addr: *listenAddr}
    var ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    go func() {
        logging.Status("listening on %s", *listenAddr)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            logging.Fatal("error listening: %v", err)
        }
    }()
    <-ctx.Done()
    stop()
    shutdown(bot, srv)
}

func shutdown(bot *bot, srv *http.Server) {
    logging.Status("shutting down")
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if err := srv.Shutdown(ctx); err != nil {
        logging.Err("webhook server shutdown: %v", err)
    }
    if dbuiSrv != nil {
        if err := dbuiSrv.Shutdown(ctx); err != nil {
            logging.Err("database UI shutdown: %v", err)
        }
    }
    if appSrv != nil {
        if err := appSrv.Shutdown(ctx); err != nil {
            logging.Err("mini app shutdown: %v", err)
        }
    }
    stopNotify()
    if err := closeDB(); err != nil {
        logging.Err("close watches database: %v", err)
    }
}

// callback handles a tapped inline-keyboard button. The button's data is the
// full id, so the lookup is just /info on it. Telegram leaves the button
// spinning until the query is answered, so that happens first and regardless of
// what the lookup does.
func callback(bot *bot, q *CallbackQuery) {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    if err := bot.answerCallback(ctx, q.ID); err != nil {
        logging.Warn("answer callback: %v", err)
    }
    cancel()
    if q.Message == nil || q.Data == "" { return }
    logging.Info("callback %q from chat %d", short(q.Data), q.Message.Chat.ID)
    info(bot, q.Message.Chat.ID, q.Data)
}

func webhookHandler(bot *bot) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
            return
        }
        if *secretToken != "" {
            var got = r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
            if subtle.ConstantTimeCompare([]byte(got), []byte(*secretToken)) != 1 {
                http.Error(w, "unauthorized", http.StatusUnauthorized)
                return
            }
        }
        defer r.Body.Close()
        var u Update
        if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
            logging.Err("decode update: %v", err)
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }
        update(bot, u)
        w.WriteHeader(http.StatusOK)
    }
}

func update(bot *bot, update Update) {
    if q := update.CallbackQuery; q != nil {
        if q.From != nil && q.From.LanguageCode != "" && q.Message != nil {
            SetChatLanguage(q.Message.Chat.ID, q.From.LanguageCode)
        }
        callback(bot, q)
        return
    }
    var msg = update.Message
    if msg == nil { return }
    var chat = msg.Chat.ID
    if msg.From != nil && msg.From.LanguageCode != "" {
        SetChatLanguage(chat, msg.From.LanguageCode)
    }
    logMessage(msg)
    var command, arg = parseCommand(msg.Text)
    switch command {
    case "/start":
        start(bot, chat)
    case "/info":
        info(bot, chat, arg)
    case "/watch":
        watchCmd(bot, chat, arg)
    case "/unwatch":
        unwatch(bot, chat, arg)
    case "/watches":
        watchesCmd(bot, chat)
    case "/fees":
        fees(bot, chat)
    case "/mempool":
        mempoolCmd(bot, chat)
    case "/miners":
        minersCmd(bot, chat)
    case "/market":
        marketCmd(bot, chat)
    case "":
        pendingInfoMu.Lock()
        var pending = pendingInfoChats[chat]
        delete(pendingInfoChats, chat)
        pendingInfoMu.Unlock()
        if pending {
            info(bot, chat, msg.Text)
            return
        }
        pendingWatchMu.Lock()
        pending = pendingWatchChats[chat]
        delete(pendingWatchChats, chat)
        pendingWatchMu.Unlock()
        if pending {
            watchCmd(bot, chat, msg.Text)
            return
        }
        pendingUnwatchMu.Lock()
        pending = pendingUnwatchChats[chat]
        delete(pendingUnwatchChats, chat)
        pendingUnwatchMu.Unlock()
        if pending {
            unwatch(bot, chat, msg.Text)
        }
    }
}

func logMessage(msg *Message) {
    var from = "unknown"
    if msg.From != nil {
        from = msg.From.Username
        if from == "" {
            from = msg.From.FirstName
        }
    }
    logging.Info("message from %s (chat %d): %s", from, msg.Chat.ID, msg.Text)
}

func isTxid(s string) bool {
    if len(s) != 64 { return false }
    var _, err = hex.DecodeString(s)
    return err == nil
}

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

func start(bot *bot, chat int64) {
    var text = i18n(chat).String("Bitnsbot. I keep an eye on the Bitcoin network for you.\n\n")
    for _, c := range commands { text += i18n(chat).String(c.line) }
    send(bot, chat, text + "\n" +
        i18n(chat).Sprintf("Version %s. Build %s. Source code <a href=\"https://github.com/pin2t/bitnsbot\">bitnsbot</a>. Don't forget to give me a ⭐",
            ver, commit), nil)
}

// menuDescription is the text Telegram shows beside a command in the menu: the
// /start line without its bullet and command prefix.
func menuDescription(line string) string {
    var _, desc, _ = strings.Cut(line, " — ")
    return strings.TrimSuffix(desc, "\n")
}

// registerMenu publishes the command menu, once per language. Telegram stores a
// separate list per language_code and falls back to the list registered without
// one, so English goes up with an empty code. A failure is logged and skipped —
// the menu is a convenience and the bot works fine without it.
func registerMenu(bot *bot) {
    var langs = []string{""}
    for lang := range langTrans { langs = append(langs, lang) }
    sort.Strings(langs)
    for _, lang := range langs {
        var cmds []map[string]string
        for _, c := range commands {
            cmds = append(cmds, map[string]string{"command": c.name,
                "description": menuDescription(langTrans[lang].String(c.line))})
        }
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var err = bot.setCommands(ctx, cmds, lang)
        cancel()
        if err != nil {
            logging.Err("register command menu (language %q): %v", lang, err)
            continue
        }
        logging.Status("command menu registered: %d commands, language %q", len(cmds), lang)
    }
}

var pendingWatchMu sync.Mutex
var pendingWatchChats = make(map[int64]bool)

// maxSubscriptionsPerChat bounds how many subscriptions (address and transaction
// watches) a single chat may hold at once. watchCmd rejects a new watch once the
// chat already has this many.
const maxSubscriptionsPerChat = 500

// atWatchLimit reports whether this chat may add another watch. The cap counts
// both kinds together: each address watch costs a live goroutine and a channel
// in the notification fan-out, and every one is woken for every match.
func atWatchLimit(chat int64) (bool, error) {
    var count, err = watches.Count(chat)
    if err != nil { return false, err }
    return count+len(txwatches.For(chat)) >= maxSubscriptionsPerChat, nil
}

// addWatch records a watch and starts everything that makes it fire: the store
// or the in-memory list, the notifier goroutine, and the local script/outpoint
// matcher. /watch and the Mini App's bell both go through here, so a watch added
// either way behaves identically.
func addWatch(b *bot, chat int64, target, alias string) error {
    if isTxid(target) {
        txwatches.Add(target, chat, alias)
    } else {
        if err := watches.Add(chat, target, alias); err != nil { return err }
        startNotifyChat(b, chat, target, alias)
        if core != nil { seedOutpoints([]string{target}) }
    }
    logging.Info("added subscription %s for chat %d (alias %q)", target, chat, alias)
    return nil
}

// removeWatch reverses addWatch and reports whether anything was actually being
// watched, which is what tells a caller "you weren't watching that".
func removeWatch(chat int64, target string) (bool, error) {
    if isTxid(target) {
        if txwatches.Remove(target, chat) == 0 { return false, nil }
        logging.Info("removed transaction watch %s for chat %d", target, chat)
        return true, nil
    }
    var removed, err = watches.Remove(chat, target)
    if err != nil { return false, err }
    if removed == 0 { return false, nil }
    stopNotifyChat(chat, target)
    txwatches.RemoveAddrConfirms(target, chat)
    unwatchScripts(target)
    logging.Info("removed subscription %s for chat %d", target, chat)
    return true, nil
}

// watching reports whether this chat already watches target, which is what the
// Mini App's bell renders as pushed or unpushed.
func watching(chat int64, target string) bool {
    if isTxid(target) {
        for _, e := range txwatches.For(chat) {
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
        if r.Chat == chat && r.Address == target { return true }
    }
    return false
}

func watchCmd(bot *bot, chat int64, arg string) {
    if arg == "" {
        pendingWatchMu.Lock()
        pendingWatchChats[chat] = true
        pendingWatchMu.Unlock()
        send(bot, chat, i18n(chat).String("Send an address or transaction to watch, optionally followed by an alias — all in one message, e.g. bc1q… John"), nil)
        return
    }
    pendingWatchMu.Lock()
    delete(pendingWatchChats, chat)
    pendingWatchMu.Unlock()
    var fields = strings.SplitN(arg, " ", 2)
    var watch = fields[0]
    var alias string
    if len(fields) > 1 { alias = strings.TrimSpace(fields[1]) }
    var full, err = atWatchLimit(chat)
    if err != nil {
        logging.Err("count watches: %v", err)
        send(bot, chat, i18n(chat).String("Sorry, something went wrong saving that watch"), nil)
        return
    }
    if full {
        logging.Info("rejected subscription for chat %d: at the limit of %d", chat, maxSubscriptionsPerChat)
        send(bot, chat, i18n(chat).Sprintf("Sorry, this chat has reached the limit of %d subscriptions. Unwatch something first to add a new one.", maxSubscriptionsPerChat), nil)
        return
    }
    if err := addWatch(bot, chat, watch, alias); err != nil {
        logging.Err("add watch: %v", err)
        send(bot, chat, i18n(chat).String("Sorry, something went wrong saving that watch"), nil)
        return
    }
    var msg string
    if alias != "" {
        msg = i18n(chat).Sprintf("Watching %s (%s)", html.EscapeString(watch), html.EscapeString(alias))
    } else {
        msg = i18n(chat).Sprintf("Watching %s", html.EscapeString(watch))
    }
    send(bot, chat, msg, nil)
}

var pendingUnwatchMu sync.Mutex
var pendingUnwatchChats = make(map[int64]bool)

func unwatch(bot *bot, chat int64, arg string) {
    if arg == "" {
        pendingUnwatchMu.Lock()
        pendingUnwatchChats[chat] = true
        pendingUnwatchMu.Unlock()
        send(bot, chat, i18n(chat).String("Send the watch you'd like to stop in a separate message"), nil)
        return
    }
    pendingUnwatchMu.Lock()
    delete(pendingUnwatchChats, chat)
    pendingUnwatchMu.Unlock()
    var removed, err = removeWatch(chat, arg)
    if err != nil {
        logging.Err("remove watch: %v", err)
        send(bot, chat, i18n(chat).String("Sorry, something went wrong removing that watch"), nil)
        return
    }
    if !removed {
        send(bot, chat, i18n(chat).Sprintf("You're not watching %s", html.EscapeString(arg)), nil)
        return
    }
    send(bot, chat, i18n(chat).Sprintf("Stopped watching %s", html.EscapeString(arg)), nil)
}

func watchesCmd(bot *bot, chat int64) {
    var records, err = watches.List()
    if err != nil {
        logging.Err("list watches: %v", err)
        send(bot, chat, i18n(chat).String("Sorry, something went wrong listing your watches"), nil)
        return
    }
    var addresses, transactions, ids []string
    for _, r := range records {
        if r.Chat != chat { continue }
        ids = append(ids, r.Address)
        var line = "<code>" + html.EscapeString(r.Address) + "</code>"
        if r.Alias != "" { line += " (" + html.EscapeString(r.Alias) + ")" }
        addresses = append(addresses, line)
    }
    for _, e := range txwatches.For(chat) {
        ids = append(ids, e.Txid)
        var line = "<code>" + html.EscapeString(e.Txid) + "</code>"
        if e.Alias != "" { line += " (" + html.EscapeString(e.Alias) + ")" }
        transactions = append(transactions, line)
    }
    if len(addresses) == 0 && len(transactions) == 0 {
        send(bot, chat, i18n(chat).String("You're not watching anything yet"), nil)
        return
    }
    var lines = []string{i18n(chat).String("Your watches:")}
    if len(addresses) > 0 {
        lines = append(lines, "", i18n(chat).String("Addresses:"))
        lines = append(lines, addresses...)
    }
    if len(transactions) > 0 {
        lines = append(lines, "", i18n(chat).String("Transactions:"))
        lines = append(lines, transactions...)
    }
    send(bot, chat, strings.Join(lines, "\n"), ids)
}

const typicalTxVsize = 140

func fees(bot *bot, chat int64) {
    if core == nil {
        send(bot, chat, i18n(chat).String("Bitcoin node connection is not configured"), nil)
        return
    }
    feesMu.Lock()
    var rec, ok, count = cachedFees, cachedFeesOK, cachedFeesCount
    feesMu.Unlock()
    if !ok {
        send(bot, chat, i18n(chat).String("Fee estimates aren't available right now — couldn't read the mempool"), nil)
        return
    }
    var tiers = []struct {
        label string
        rate  float64
    }{
        {i18n(chat).String("Fastest (10-20 min):"), rec.fastest},
        {i18n(chat).String("Medium (~1 h):"),       rec.hour},
        {i18n(chat).String("Minimum (2+ h):"),      rec.minimum},
    }
    var pad int
    for _, t := range tiers { pad = max(pad, utf8.RuneCountInString(t.label)+1) }
    var price, havePrice = rates.Last()
    var lines []string
    for _, t := range tiers {
        var cell = trimNum(t.rate, 2) + i18n(chat).String(" sat/vB")
        if havePrice {
            cell += "  (≈ " + usd(int64(math.Round(t.rate*typicalTxVsize)), price) + ")"
        }
        lines = append(lines, fmt.Sprintf("%-*s %s", pad, t.label, cell))
    }
    var msg = i18n(chat).Sprintf("Network fees projected from %s mempool transactions", group(int64(count)))
    if havePrice {
        msg += ". " + i18n(chat).Sprintf("USD for a typical %d vB transaction", typicalTxVsize)
    }
    send(bot, chat, msg + "\n\n<pre>" + strings.Join(lines, "\n") + "</pre>", nil)
}

// flowInterval is how often startMempoolFlow polls the mempool tx count. A
// package var so tests can shrink it.
var flowInterval = 10 * time.Second

// The mempool flow rate — transactions per second flowing into the mempool, as
// Δcount over the poll interval — and how much that rate changed since the last
// poll. In-memory only (guarded by flowMu); never persisted.
var flowMu sync.Mutex
var flowRate float64    // current tx/sec
var flowRateOK bool     // a rate has been computed (≥ 2 samples)
var flowChange float64  // Δ rate since the previous rate
var flowChangeOK bool   // a change has been computed (≥ 2 rates)
var flowPrevCount int64
var flowHaveCount bool

// mempool summary values — total output amount and total fees across the whole
// mempool, recomputed every 10 minutes in the background and printed by /mempool
// with a tilde prefix to mark them as cached, not fresh.
var summaryMu sync.Mutex
var summaryAmount int64
var summaryFee int64
var summaryOK bool

// cached fees recommendation, recomputed every 10 minutes alongside
// startMempoolFees so /fees can reply instantly and confEstimate can
// pick up the current fee landscape without an RPC call.
var feesMu sync.Mutex
var cachedFees recommendedFees
var cachedFeesOK bool
var cachedFeesCount int // number of mempool entries the fees were projected from

// updateFlow folds one mempool tx-count sample into the flow-rate state: the rate
// is Δcount over flowInterval, and the change is Δrate (flowRate still holds the
// previous rate when this runs). The first sample only sets the baseline. A
// non-increase (count ≤ prev — most likely a just-mined block clearing the
// mempool, which would skew the inflow measurement) keeps the last rate/change,
// but still re-baselines the count so the next increase spans one clean interval.
func updateFlow(count int64) {
    flowMu.Lock()
    defer flowMu.Unlock()
    if !flowHaveCount {
        flowPrevCount, flowHaveCount = count, true
        return
    }
    var delta = count - flowPrevCount
    flowPrevCount = count
    if delta <= 0 { return }
    var rate = float64(delta) / flowInterval.Seconds()
    if flowRateOK {
        flowChange, flowChangeOK = rate-flowRate, true
    }
    flowRate, flowRateOK = rate, true
}

// startMempoolFlow polls getmempoolinfo every flowInterval and feeds the tx count
// to updateFlow, so /mempool can show a live flow rate. The goroutine isn't
// stopped on shutdown — like the rates updater, the process exits right after.
func startMempoolFlow() {
    if core == nil { return }
    go func() {
        var sample = func() {
            var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
            var info, err = core.getMempoolInfo(ctx)
            cancel()
            if err != nil {
                logging.Warn("mempool flow: %v", err)
                return
            }
            updateFlow(info.Size)
        }
        sample()
        var t = time.NewTicker(flowInterval)
        defer t.Stop()
        for range t.C {
            sample()
        }
    }()
}

// startMempoolSummary recomputes the mempool totals (output amount + fees) every
// 10 minutes in the background and stores them so /mempool can reply instantly
// with a ~-prefixed cached value instead of summing on every request.
func startMempoolSummary() {
    if core == nil { return }
    go func() {
        var calc = func() {
            var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
            defer cancel()
            var amount, fee, ok = mempoolTotals(ctx)
            summaryMu.Lock()
            summaryAmount, summaryFee, summaryOK = amount, fee, ok
            summaryMu.Unlock()
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

func startNetworkStats() {
    if core == nil { return }
    go func() {
        refreshNetwork(true)
        var t = time.NewTicker(10 * time.Minute)
        defer t.Stop()
        for range t.C {
            refreshNetwork(true)
        }
    }()
}

// refreshNetwork rebuilds the Blockchain card and pushes it to any open page.
//
// withNodes gates the one expensive call: getnodeaddresses returns every address
// the node knows — tens of thousands of entries and several megabytes on mainnet
// — and the count it produces barely moves between blocks. The ticker asks for
// it; a new block does not, and carries the last known count forward instead.
func refreshNetwork(withNodes bool) {
    if core == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    var info, err = core.getBlockchainInfo(ctx)
    if err != nil {
        logging.Warn("network stats: %v", err)
        return
    }
    networkMu.Lock()
    var nodes, txs = cachedNetwork.Nodes, cachedNetwork.Txs
    networkMu.Unlock()
    // Same rule as the peer count: a failure here keeps the last known figure
    // rather than blanking the field.
    if stats, serr := core.getChainTxStats(ctx); serr == nil {
        txs = bigCount(stats.TxCount)
    } else {
        logging.Warn("network stats: chain tx stats: %v", serr)
        if txs == "" { txs = "—" }
    }
    // A failed peer count must not blank the field or, worse, report 0 active
    // nodes — keep whatever we last knew.
    if withNodes {
        if addrs, aerr := core.getNodeAddresses(ctx); aerr == nil {
            var cutoff = time.Now().Add(-activeNodeWindow).Unix()
            var active int64
            for _, a := range addrs {
                if a.Time >= cutoff { active++ }
            }
            nodes = group(active)
        } else {
            logging.Warn("network stats: node addresses: %v", aerr)
        }
    }
    if nodes == "" { nodes = "—" }
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true,
        Coins:  metric(toBTC(circulatingSupply(info.Blocks)), 1),
        Cap:    "21 M",
        Blocks: group(info.Blocks),
        Size:   humSize(info.SizeOnDisk, 0, 0),
        Nodes:  nodes,
        Txs:    txs,
        // A fixed figure: nothing counts distinct addresses yet. The addrindex
        // package records touches per address but keeps no distinct total, so
        // this stands in until it can.
        Addresses: "1.5 B",
    }
    networkMu.Unlock()
    app.Notify("network")
}

func startMempoolFees() {
    if core == nil { return }
    go func() {
        refreshFees()
        var t = time.NewTicker(10 * time.Minute)
        defer t.Stop()
        for range t.C {
            refreshFees()
        }
    }()
}

// refreshFees recomputes the projection and pushes it to any open page.
func refreshFees() {
    if core == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    var entries, err = core.rawMempoolVerbose(ctx)
    if err != nil {
        logging.Warn("fees: %v", err)
        return
    }
    var minFee float64
    if info, ierr := core.getMempoolInfo(ctx); ierr == nil {
        minFee = info.MempoolMinFee
    }
    var rec = calculateRecommendedFee(buildProjectedBlocks(entries), minFee)
    feesMu.Lock()
    cachedFees, cachedFeesOK, cachedFeesCount = rec, true, len(entries)
    feesMu.Unlock()
    app.Notify("fees")
}

// onNewBlock refreshes what a block actually changes, so the Mini App updates
// within seconds of one being mined rather than waiting out a ten-minute ticker.
// A block moves the height and, by clearing the mempool, the fee projection —
// which is at its most stale right after one. The peer count is left to the
// ticker; it is the expensive call and it barely moves block to block.
func onNewBlock() {
    refreshNetwork(false)
    refreshFees()
}

// mempoolSummaryLimit caps how many transactions /mempool will total up — above
// it, the totals (which need the whole verbose mempool plus a fetch per tx) would
// take too long, so the reply degrades to just size and count. A package var so
// it's tunable/testable.
var mempoolSummaryLimit int64 = 20000

// mempoolCmd replies with the current mempool size and transaction count, plus —
// when the mempool is small enough to total up in reasonable time — the summed
// output amount and summed fees of every mempool transaction, in sats and USD.
func mempoolCmd(bot *bot, chat int64) {
    if core == nil {
        send(bot, chat, i18n(chat).String("Bitcoin node connection is not configured"), nil)
        return
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    var info, err = core.getMempoolInfo(ctx)
    if err != nil {
        logging.Err("get mempool info: %v", err)
        send(bot, chat, i18n(chat).String("Sorry, something went wrong reading the mempool"), nil)
        return
    }
    var pairs = [][2]string{
        {i18n(chat).String("Size"),         humSize(info.Bytes, 2, chat)},
        {i18n(chat).String("Transactions"), group(int64(info.Size))},
    }
    flowMu.Lock()
    var rate, rateOK, change, changeOK = flowRate, flowRateOK, flowChange, flowChangeOK
    flowMu.Unlock()
    if rateOK {
        var fr = i18n(chat).Sprintf("%.1f tx/sec", rate)
        if changeOK {
            fr += fmt.Sprintf(" (%+.1f)", change)
        }
        pairs = append(pairs, [2]string{i18n(chat).String("Flow rate"), fr})
    }
    if info.Size > 0 {
        summaryMu.Lock()
        var amount, fee, ok = summaryAmount, summaryFee, summaryOK
        summaryMu.Unlock()
        if ok {
            pairs = append(pairs,
                [2]string{i18n(chat).String("Total flow"), "~" + btcAmount(amount)},
                [2]string{i18n(chat).String("Total fees"), "~" + btcAmount(fee)},
            )
        }
    }
    send(bot, chat, i18n(chat).Sprintf("Mempool\n\n<pre>%s</pre>", joinAlign(pairs)), nil)
}

// mempoolTotals sums the fee (from the verbose mempool) and the output amount
// (fetching each transaction, concurrently and bounded) across the whole
// mempool. Individual tx churn — a tx confirmed or replaced between the two
// passes — is tolerated (its outputs are just skipped); a context timeout means
// the mempool was too large and returns ok=false so the reply shows "unavailable".
func mempoolTotals(ctx context.Context) (amount, fee int64, ok bool) {
    var mp, err = core.rawMempoolVerbose(ctx)
    if err != nil { return 0, 0, false }
    var txids = make([]string, 0, len(mp))
    for id, e := range mp {
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
            var tx, e = core.getRawTransaction(ctx, id)
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
func minersCmd(bot *bot, chat int64) {
    var top = miners.Top(10)
    if len(top) == 0 {
        send(bot, chat, i18n(chat).String("No miner statistics yet — still collecting"), nil)
        return
    }
    var lines []string
    for i, m := range top {
        var blocks = i18n(chat).Sprintf("%d blocks", m.Blocks)
        if m.Blocks == 1 { blocks = i18n(chat).String("1 block") }
        lines = append(lines, i18n(chat).Sprintf("%d. %s. %s mined, reward %s BTC, fees %s BTC, consumption %s GW",
            i+1, m.Name, blocks, trimNum(toBTC(m.Reward), 2), trimNum(toBTC(m.Fees), 2), trimNum(m.ConsumptionGW, 1)))
    }
    send(bot, chat, i18n(chat).String("Top miners by blocks mined:") + "\n\n" + strings.Join(lines, "\n"), nil)
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
func marketCmd(bot *bot, chat int64) {
    var now, haveNow = rates.Last()
    var snapshot, haveSnapshot = rates.LastMarket()
    if haveSnapshot && snapshot.Price > 0 {
        now, haveNow = snapshot.Price, true
    }
    if !haveNow {
        send(bot, chat, i18n(chat).String("No price data yet — still fetching"), nil)
        return
    }
    var pairs = [][2]string{{i18n(chat).String("Price"), price(now)}}
    if haveSnapshot && snapshot.MarketCap > 0 {
        pairs = append(pairs, [2]string{i18n(chat).String("Market cap"), money(snapshot.MarketCap, chat)})
    } else {
        pairs = append(pairs, [2]string{i18n(chat).String("Market cap"), i18n(chat).String("unavailable")})
    }
    if haveSnapshot && snapshot.Volume24h > 0 {
        pairs = append(pairs, [2]string{i18n(chat).String("Volume 24h"), money(snapshot.Volume24h, chat)})
    } else {
        pairs = append(pairs, [2]string{i18n(chat).String("Volume 24h"), i18n(chat).String("unavailable")})
    }
    var periods = []struct {
        label string
        back  time.Duration
    }{
        {i18n(chat).String("1 day"),    24 * time.Hour},
        {i18n(chat).String("1 week"),   7 * 24 * time.Hour},
        {i18n(chat).String("1 month"),  30 * 24 * time.Hour},
        {i18n(chat).String("3 months"), 90 * 24 * time.Hour},
        {i18n(chat).String("1 year"),   365 * 24 * time.Hour},
        {i18n(chat).String("5 years"),  5 * 365 * 24 * time.Hour},
    }
    var changes [][2]string
    for _, p := range periods {
        if then, ok := rates.At(time.Now().Add(-p.back)); ok && then > 0 {
            changes = append(changes, [2]string{p.label, change(now, then)})
        }
    }
    if len(changes) > 0 {
        pairs = append(pairs, [2]string{i18n(chat).String("Changes"), ""})
        for _, c := range changes { pairs = append(pairs, c) }
    }
    send(bot, chat, i18n(chat).Sprintf("Bitcoin market\n\n<pre>%s</pre>", joinAlign(pairs)), nil)
}

func send(bot *bot, chat int64, text string, ids []string) {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := bot.sendWithButtons(ctx, chat, text, ids); err != nil {
        logging.Err("send message: %v", err)
        return
    }
    logging.Info("sent message to chat %d", chat)
}
