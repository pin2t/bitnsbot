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
import "strconv"
import "strings"
import "sync"
import "syscall"
import "time"
import "runtime/debug"
import "bitnsbot/dbui"
import "bitnsbot/logging"
import "bitnsbot/miners"
import "bitnsbot/rates"
import "bitnsbot/txwatches"
import "bitnsbot/watches"

var configPath      = flag.String("config", "", "path to a properties file (name=value lines) with flag values; command-line flags take precedence")
var verbose         = flag.Int("verbose", 0, "log verbosity: 0=ERR/WARN/status, 1=+INFO, 2=+NET/DB (raw external traffic and storage requests)")
var botToken        = flag.String("bot-token", "", "Telegram bot token authenticating outbound Bot API calls (required)")
var listenAddr      = flag.String("listen", ":8080", "listen address")
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
var logNoTs = flag.Bool("log-no-ts", false, "omit the date and time prefix from each log line")
var dbuiListen      = flag.String("dbui-listen", "", "address for the database admin web UI, e.g. 127.0.0.1:8090 (empty disables it; bind to localhost only — it can write any bucket)")

var core *coreClient
var dbuiSrv *http.Server
var ver = "1.0"

func main() {
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
        flag.PrintDefaults()
    }
    flag.Parse()
    if *configPath != "" {
        if err := applyConfig(*configPath); err != nil {
            logging.Fatal("apply config: %v", err)
        }
    }
    logging.SetVerbosity(*verbose)
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
    rates.Start()
    if *dbuiListen != "" {
        dbuiSrv = dbui.Start(db, *dbuiListen)
    }
    if *backupPath != "" {
        startBackup(*backupPath, *backupInterval, *backupScript)
        logging.Status("backing up the database to %s every %s", *backupPath, *backupInterval)
    }
    if *coreURL != "" {
        core, err = newCoreClient(coreConfig{
            url:        *coreURL,
            user:       *coreUser,
            pass:       *corePass,
            cookieFile: *coreCookie,
        })
        if err != nil {
            logging.Fatal("Bitcoin Core client: %v", err)
        }
        // unlike btcd's websocket there is no connection to establish or
        // supervise — every call is its own HTTP request — so a bad URL or
        // credentials surface on the first real call instead of at startup.
        // Check once here so that failure is loud and immediate.
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
    shutdown(srv)
}

func shutdown(srv *http.Server) {
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
        callback(bot, q)
        return
    }
    var msg = update.Message
    if msg == nil { return }
    logMessage(msg)
    var command, arg = parseCommand(msg.Text)
    switch command {
    case "/start":
        start(bot, msg.Chat.ID)
    case "/info":
        info(bot, msg.Chat.ID, arg)
    case "/watch":
        watchCmd(bot, msg.Chat.ID, arg)
    case "/unwatch":
        unwatch(bot, msg.Chat.ID, arg)
    case "/watches":
        watchesCmd(bot, msg.Chat.ID)
    case "/fees":
        fees(bot, msg.Chat.ID)
    case "/mempool":
        mempoolCmd(bot, msg.Chat.ID)
    case "/miners":
        minersCmd(bot, msg.Chat.ID)
    case "/market":
        marketCmd(bot, msg.Chat.ID)
    case "":
        pendingInfoMu.Lock()
        var pending = pendingInfoChats[msg.Chat.ID]
        delete(pendingInfoChats, msg.Chat.ID)
        pendingInfoMu.Unlock()
        if pending {
            info(bot, msg.Chat.ID, msg.Text)
            return
        }
        pendingWatchMu.Lock()
        pending = pendingWatchChats[msg.Chat.ID]
        delete(pendingWatchChats, msg.Chat.ID)
        pendingWatchMu.Unlock()
        if pending {
            watchCmd(bot, msg.Chat.ID, msg.Text)
            return
        }
        pendingUnwatchMu.Lock()
        pending = pendingUnwatchChats[msg.Chat.ID]
        delete(pendingUnwatchChats, msg.Chat.ID)
        pendingUnwatchMu.Unlock()
        if pending {
            unwatch(bot, msg.Chat.ID, msg.Text)
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
    if len(s) != 64 {
        return false
    }
    var _, err = hex.DecodeString(s)
    return err == nil
}

func parseCommand(text string) (command, arg string) {
    var fields = strings.SplitN(strings.TrimSpace(text), " ", 2)
    if !strings.HasPrefix(fields[0], "/") {
        return "", ""
    }
    command = strings.SplitN(fields[0], "@", 2)[0]
    if len(fields) > 1 {
        arg = strings.TrimSpace(fields[1])
    }
    return command, arg
}

func start(bot *bot, chatID int64) {
    var b, _ = debug.ReadBuildInfo()
    var commit = ""
    if b != nil {
        for _, s := range b.Settings {
            if s.Key == "vcs.revision" { commit = "Build " + s.Value[:6] }
        }
    }
    send(bot, chatID, strings.Join([]string{
        "Bitnsbot. I keep an eye on the Bitcoin network for you.",
        "",
        "• <b>/info</b> — look up a transaction, block, or address",
        "• <b>/watch</b> — get notified when an address receives a payment, or when a transaction confirms",
        "• <b>/unwatch</b> — stop watching an address or transaction",
        "• <b>/watches</b> — list what you're currently watching",
        "• <b>/fees</b> — show current network fee estimates",
        "• <b>/mempool</b> — show current mempool size and totals",
        "• <b>/miners</b> — top mining pools by blocks mined",
        "• <b>/market</b> — price, market cap, volume and recent changes",
        "• <b>/start</b> — show this message",
        "",
        "Version " + ver + ". " + commit + ". Source code on <a href=\"https://github.com/pin2t/bitnsbot\">Github</a>. " +
        "Don't forget to give me a ⭐",
    }, "\n"))
}

var pendingWatchMu sync.Mutex
var pendingWatchChats = make(map[int64]bool)

func watchCmd(bot *bot, chatID int64, arg string) {
    if arg == "" {
        pendingWatchMu.Lock()
        pendingWatchChats[chatID] = true
        pendingWatchMu.Unlock()
        send(bot, chatID, "Send an address or transaction to watch, optionally followed by an alias — all in one message, e.g. bc1q… John")
        return
    }
    pendingWatchMu.Lock()
    delete(pendingWatchChats, chatID)
    pendingWatchMu.Unlock()
    var fields = strings.SplitN(arg, " ", 2)
    var watchID = fields[0]
    var alias string
    if len(fields) > 1 {
        alias = strings.TrimSpace(fields[1])
    }
    var typ = watchTypeAddress
    if isTxid(watchID) {
        typ = watchTypeTransaction
    }
    if typ == watchTypeTransaction {
        txwatches.Add(watchID, chatID, alias)
    } else {
        if err := watches.Add(chatID, watchID, alias); err != nil {
            logging.Err("add watch: %v", err)
            send(bot, chatID, "Sorry, something went wrong saving that watch.")
            return
        }
        startNotifyChat(bot, chatID, typ, watchID, alias)
        if core != nil {
            // seedOutpoints registers the address's scriptPubKey and its current
            // UTXOs with the local matcher; there is no node-side filter to load
            seedOutpoints([]string{watchID})
        }
    }
    logging.Info("added %s subscription %s for chat %d (alias %q)", typ, watchID, chatID, alias)
    var msg = "Watching " + string(typ) + ": " + html.EscapeString(watchID)
    if alias != "" {
        msg += " (" + html.EscapeString(alias) + ")"
    }
    send(bot, chatID, msg)
}

var pendingUnwatchMu sync.Mutex
var pendingUnwatchChats = make(map[int64]bool)

func unwatch(bot *bot, chatID int64, arg string) {
    if arg == "" {
        pendingUnwatchMu.Lock()
        pendingUnwatchChats[chatID] = true
        pendingUnwatchMu.Unlock()
        send(bot, chatID, "Send the watch you'd like to stop in a separate message.")
        return
    }
    pendingUnwatchMu.Lock()
    delete(pendingUnwatchChats, chatID)
    pendingUnwatchMu.Unlock()
    if isTxid(arg) {
        if txwatches.Remove(arg, chatID) == 0 {
            send(bot, chatID, "You're not watching "+html.EscapeString(arg)+".")
            return
        }
        logging.Info("removed transaction watch %s for chat %d", arg, chatID)
        send(bot, chatID, "Stopped watching "+html.EscapeString(arg)+".")
        return
    }
    var removed, err = watches.Remove(chatID, arg)
    if err != nil {
        logging.Err("remove watch: %v", err)
        send(bot, chatID, "Sorry, something went wrong removing that watch.")
        return
    }
    if removed == 0 {
        send(bot, chatID, "You're not watching "+html.EscapeString(arg)+".")
        return
    }
    stopNotifyChat(chatID, watchTypeAddress, arg)
    txwatches.RemoveAddrConfirms(arg, chatID)
    logging.Info("removed subscription %s for chat %d", arg, chatID)
    unwatchScripts(arg)
    send(bot, chatID, "Stopped watching "+html.EscapeString(arg)+".")
}

func watchesCmd(bot *bot, chatID int64) {
    var records, err = watches.List()
    if err != nil {
        logging.Err("list watches: %v", err)
        send(bot, chatID, "Sorry, something went wrong listing your watches.")
        return
    }
    var addresses, transactions, ids []string
    for _, r := range records {
        if r.ChatID != chatID {
            continue
        }
        ids = append(ids, r.Address)
        var line = "<code>" + html.EscapeString(r.Address) + "</code>"
        if r.Alias != "" {
            line += " (" + html.EscapeString(r.Alias) + ")"
        }
        addresses = append(addresses, line)
    }
    for _, e := range txwatches.For(chatID) {
        ids = append(ids, e.Txid)
        var line = "<code>" + html.EscapeString(e.Txid) + "</code>"
        if e.Alias != "" {
            line += " (" + html.EscapeString(e.Alias) + ")"
        }
        transactions = append(transactions, line)
    }
    if len(addresses) == 0 && len(transactions) == 0 {
        send(bot, chatID, "You're not watching anything yet.")
        return
    }
    var lines = []string{"Your watches:"}
    if len(addresses) > 0 {
        lines = append(lines, "", "Addresses:")
        lines = append(lines, addresses...)
    }
    if len(transactions) > 0 {
        lines = append(lines, "", "Transactions:")
        lines = append(lines, transactions...)
    }
    sendLinked(bot, chatID, strings.Join(lines, "\n"), ids)
}

// fees replies with current network fee estimates for three confirmation
// speeds. core's estimatefee returns BTC/kB, converted to sat/vB (×1e5); a tier
// core can't estimate yet shows "n/a", and if none are available the whole
// reply degrades to a short "not available yet" note. The three per-tier calls
// run concurrently (jsonrpc2 multiplexes them over the one connection) so the
// reply waits on the slowest rather than the sum.
func fees(bot *bot, chatID int64) {
    if core == nil {
        send(bot, chatID, "Bitcoin node connection is not configured.")
        return
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var tiers = []struct {
        label  string
        blocks int
    }{
        {"Fast (10-20 min):", 2},
        {"Medium (~1h):", 6},
        {"Slow (2h+):", 12},
    }
    var results = make([]struct {
        btcPerKvB float64
        err      error
    }, len(tiers))
    var wg sync.WaitGroup
    for i, t := range tiers {
        wg.Add(1)
        go func(i, blocks int) {
            defer wg.Done()
            results[i].btcPerKvB, results[i].err = core.estimateSmartFee(ctx, int64(blocks))
        }(i, t.blocks)
    }
    wg.Wait()
    var pad int
    for _, t := range tiers {
        if len(t.label) > pad {
            pad = len(t.label)
        }
    }
    var lines []string
    var available bool
    for i, t := range tiers {
        if results[i].err != nil || results[i].btcPerKvB <= 0 {
            lines = append(lines, fmt.Sprintf("%-*s  n/a", pad, t.label))
            continue
        }
        available = true
        var rate = strings.TrimSuffix(strconv.FormatFloat(results[i].btcPerKvB*1e5, 'f', 1, 64), ".0")
        lines = append(lines, fmt.Sprintf("%-*s  %s sat/vB", pad, t.label, rate))
    }
    if !available {
        send(bot, chatID, "Fee estimates aren't available yet — the node hasn't observed enough network activity.")
        return
    }
    send(bot, chatID, "Estimated network fees\n\n<pre>"+strings.Join(lines, "\n")+"</pre>")
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
var summaryAmount float64
var summaryFee float64
var summaryOK bool

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
    if delta <= 0 {
        return
    }
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
    if core == nil {
        return
    }
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
    if core == nil {
        return
    }
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

// mempoolSummaryLimit caps how many transactions /mempool will total up — above
// it, the totals (which need the whole verbose mempool plus a fetch per tx) would
// take too long, so the reply degrades to just size and count. A package var so
// it's tunable/testable.
var mempoolSummaryLimit int64 = 20000

// mempoolCmd replies with the current mempool size and transaction count, plus —
// when the mempool is small enough to total up in reasonable time — the summed
// output amount and summed fees of every mempool transaction, in sats and USD.
func mempoolCmd(bot *bot, chatID int64) {
    if core == nil {
        send(bot, chatID, "Bitcoin node connection is not configured.")
        return
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    var info, err = core.getMempoolInfo(ctx)
    if err != nil {
        logging.Err("get mempool info: %v", err)
        send(bot, chatID, "Sorry, something went wrong reading the mempool.")
        return
    }
    var pairs = [][2]string{
        {"Size", metric(float64(info.Bytes), 1)},
        {"Transactions", metric(float64(info.Size), 1)},
    }
    flowMu.Lock()
    var rate, rateOK, change, changeOK = flowRate, flowRateOK, flowChange, flowChangeOK
    flowMu.Unlock()
    if rateOK {
        var fr = fmt.Sprintf("%.1f tx/sec", rate)
        if changeOK {
            fr += fmt.Sprintf(" (%+.1f)", change)
        }
        pairs = append(pairs, [2]string{"Flow rate", fr})
    }
    switch {
    case info.Size == 0:
        // nothing to total
    default:
        summaryMu.Lock()
        var amount, fee, ok = summaryAmount, summaryFee, summaryOK
        summaryMu.Unlock()
        if ok {
            pairs = append(pairs,
                [2]string{"Total flow", cachedBtc(amount)},
                [2]string{"Total fees", cachedBtc(fee)},
            )
        }
    }
    var pad int
    for _, p := range pairs {
        if len(p[0])+1 > pad { pad = len(p[0]) + 1 }
    }
    var lines []string
    for _, p := range pairs {
        lines = append(lines, fmt.Sprintf("%-*s %s", pad, p[0]+":", p[1]))
    }
    send(bot, chatID, "Mempool\n\n<pre>"+strings.Join(lines, "\n")+"</pre>")
}

// mempoolTotals sums the fee (from the verbose mempool) and the output amount
// (fetching each transaction, concurrently and bounded) across the whole
// mempool. Individual tx churn — a tx confirmed or replaced between the two
// passes — is tolerated (its outputs are just skipped); a context timeout means
// the mempool was too large and returns ok=false so the reply shows "unavailable".
func mempoolTotals(ctx context.Context) (amount, fee float64, ok bool) {
    var mp, err = core.rawMempoolVerbose(ctx)
    if err != nil {
        return 0, 0, false
    }
    var txids = make([]string, 0, len(mp))
    for id, e := range mp {
        fee += e.Fees.Base
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
            if e != nil {
                return
            }
            var out float64
            for _, v := range tx.Vout {
                out += v.Value
            }
            mu.Lock()
            amount += out
            mu.Unlock()
        }(id)
    }
    wg.Wait()
    if ctx.Err() != nil {
        return 0, 0, false
    }
    return amount, fee, true
}

// minersCmd replies with the top 10 mining pools by blocks mined over the window
// the stats collector has processed, each with its total reward, fees, and an
// estimated power draw (see the miners package).
func minersCmd(bot *bot, chatID int64) {
    var top = miners.Top(10)
    if len(top) == 0 {
        send(bot, chatID, "No miner statistics yet — still collecting.")
        return
    }
    var lines []string
    for i, m := range top {
        var blocks = fmt.Sprintf("%d blocks", m.Blocks)
        if m.Blocks == 1 { blocks = "1 block" }
        lines = append(lines, fmt.Sprintf("%d. %s. %s mined, reward %s BTC, fees %s BTC, consumption %s GW",
            i+1, m.Name, blocks, trimNum(m.Reward, 2), trimNum(m.Fees, 2), trimNum(m.ConsumptionGW, 1)))
    }
    send(bot, chatID, "Top miners by blocks mined:\n\n"+strings.Join(lines, "\n"))
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
func marketCmd(bot *bot, chatID int64) {
    var now, haveNow = rates.Last()
    var snapshot, haveSnapshot = rates.LastMarket()
    if haveSnapshot && snapshot.Price > 0 {
        now, haveNow = snapshot.Price, true
    }
    if !haveNow {
        send(bot, chatID, "No price data yet — still fetching.")
        return
    }
    var pairs = [][2]string{{"Price", price(now)}}
    if haveSnapshot && snapshot.MarketCap > 0 {
        pairs = append(pairs, [2]string{"Market cap", money(snapshot.MarketCap)})
    } else {
        pairs = append(pairs, [2]string{"Market cap", "unavailable"})
    }
    if haveSnapshot && snapshot.Volume24h > 0 {
        pairs = append(pairs, [2]string{"Volume 24h", money(snapshot.Volume24h)})
    } else {
        pairs = append(pairs, [2]string{"Volume 24h", "unavailable"})
    }
    var periods = []struct {
        label string
        back  time.Duration
    }{
        {"24h", 24 * time.Hour},
        {"1w", 7 * 24 * time.Hour},
        {"1m", 30 * 24 * time.Hour},
        {"3m", 90 * 24 * time.Hour},
        {"1y", 365 * 24 * time.Hour},
        {"5y", 5 * 365 * 24 * time.Hour},
    }
    var changes [][2]string
    for _, p := range periods {
        if then, ok := rates.At(time.Now().Add(-p.back)); ok && then > 0 {
            changes = append(changes, [2]string{p.label, change(now, then)})
        }
    }
    var pad int
    for _, p := range pairs {
        if len(p[0])+1 > pad { pad = len(p[0]) + 1 }
    }
    var lines []string
    for _, p := range pairs {
        lines = append(lines, fmt.Sprintf("%-*s %s", pad, p[0]+":", p[1]))
    }
    if len(changes) > 0 {
        var cpad int
        for _, c := range changes {
            if len(c[0])+1 > cpad { cpad = len(c[0]) + 1 }
        }
        lines = append(lines, "", "Changes")
        for _, c := range changes {
            lines = append(lines, fmt.Sprintf("%-*s %s", cpad, c[0]+":", c[1]))
        }
    }
    send(bot, chatID, "Bitcoin market\n\n<pre>"+strings.Join(lines, "\n")+"</pre>")
}

func send(bot *bot, chatID int64, text string) {
    sendLinked(bot, chatID, text, nil)
}

// sendLinked sends a message that carries a button per id, so every shortened id
// in the text is one tap away from its own /info lookup.
func sendLinked(bot *bot, chatID int64, text string, ids []string) {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := bot.sendWithButtons(ctx, chatID, text, ids); err != nil {
        logging.Err("send message: %v", err)
        return
    }
    logging.Info("sent message to chat %d", chatID)
}
