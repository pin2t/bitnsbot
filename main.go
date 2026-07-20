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
var btcdURL         = flag.String("btcd-url", "", "btcd RPC websocket URL, e.g. wss://localhost:8334/ws (leave empty to skip connecting to btcd)")
var btcdUser        = flag.String("btcd-user", "", "btcd RPC username")
var btcdPass        = flag.String("btcd-pass", "", "btcd RPC password")
var btcdCert        = flag.String("btcd-cert", "", "path to btcd's rpc.cert for self-signed TLS trust")
var btcdInsecureTLS = flag.Bool("btcd-insecure-tls", false, "skip TLS certificate verification for the btcd connection (dev only)")
var backupPath      = flag.String("backup", "", "path to copy the database to periodically (empty disables backups)")
var backupInterval  = flag.Duration("backup-interval", 24*time.Hour, "how often to back up the database")
var backupScript    = flag.String("backup-script", "", "command run after each backup, with the backup's path as $1 and in $BACKUP_FILE (empty runs nothing)")

var btcd *btcdClient

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
    if *botToken == "" {
        logging.Fatal("-bot-token is required")
    }
    var bot = newBot(*botToken, *apiBaseURL)
    var err error
    if err = openDB(*dbPath); err != nil {
        logging.Fatal("open database: %v", err)
    }
    rates.Start()
    if *backupPath != "" {
        startBackup(*backupPath, *backupInterval, *backupScript)
        logging.Status("backing up the database to %s every %s", *backupPath, *backupInterval)
    }
    if *btcdURL != "" {
        var btcdCtx, btcdCancel = context.WithTimeout(context.Background(), 15*time.Second)
        btcd, err = dialBtcd(btcdCtx, btcdConfig{
            url:         *btcdURL,
            user:        *btcdUser,
            pass:        *btcdPass,
            certFile:    *btcdCert,
            insecureTLS: *btcdInsecureTLS,
        }, notifier{bot: bot})
        btcdCancel()
        if err != nil {
            logging.Fatal("dial btcd: %v", err)
        }
        logging.Status("connected to btcd at %s", *btcdURL)
    }
    startNotify(bot)
    miners.Start()
    if btcd != nil {
        miners.StartStats(minerSource{})
    }
    startBlockCache()
    startMempoolFlow()
    if btcd != nil {
        btcd.supervise(reapplyBtcdState)
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
    if btcd != nil {
        if err := btcd.close(); err != nil {
            logging.Err("close btcd: %v", err)
        }
    }
    stopNotify()
    if err := closeDB(); err != nil {
        logging.Err("close watches database: %v", err)
    }
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
    send(bot, chatID, strings.Join([]string{
        "Hi! I'm bitnsbot — I keep an eye on the Bitcoin network for you.",
        "",
        "• <b>/info</b> — look up a transaction, block, or address",
        "• <b>/watch</b> — get notified when an address receives a payment, or when a transaction confirms",
        "• <b>/unwatch</b> — stop watching an address or transaction",
        "• <b>/watches</b> — list what you're currently watching",
        "• <b>/fees</b> — show current network fee estimates",
        "• <b>/mempool</b> — show current mempool size and totals",
        "• <b>/miners</b> — top mining pools by blocks mined",
        "• <b>/start</b> — show this message",
    }, "\n"))
}

var pendingWatchMu sync.Mutex
var pendingWatchChats = make(map[int64]bool)

func watchCmd(bot *bot, chatID int64, arg string) {
    if arg == "" {
        pendingWatchMu.Lock()
        pendingWatchChats[chatID] = true
        pendingWatchMu.Unlock()
        send(bot, chatID, "Please send the address or transaction to watch, optionally followed by an alias — all in one message, e.g. bc1q… John")
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
        if btcd != nil {
            var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
            if err := btcd.loadTxFilter(ctx, false, []string{watchID}, nil); err != nil {
                logging.Warn("load tx filter: %v", err)
            }
            cancel()
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
        send(bot, chatID, "Please send the watch you'd like to stop in a separate message.")
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
    if btcd != nil {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        if err := btcd.loadTxFilter(ctx, true, notifyAddresses(), nil); err != nil {
            logging.Warn("load tx filter: %v", err)
        }
        cancel()
    }
    send(bot, chatID, "Stopped watching "+html.EscapeString(arg)+".")
}

func watchesCmd(bot *bot, chatID int64) {
    var records, err = watches.List()
    if err != nil {
        logging.Err("list watches: %v", err)
        send(bot, chatID, "Sorry, something went wrong listing your watches.")
        return
    }
    var addresses, transactions []string
    for _, r := range records {
        if r.ChatID != chatID {
            continue
        }
        var line = "<code>" + html.EscapeString(r.Address) + "</code>"
        if r.Alias != "" {
            line += " (" + html.EscapeString(r.Alias) + ")"
        }
        addresses = append(addresses, line)
    }
    for _, e := range txwatches.For(chatID) {
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
    send(bot, chatID, strings.Join(lines, "\n"))
}

// fees replies with current network fee estimates for three confirmation
// speeds. btcd's estimatefee returns BTC/kB, converted to sat/vB (×1e5); a tier
// btcd can't estimate yet shows "n/a", and if none are available the whole
// reply degrades to a short "not available yet" note. The three per-tier calls
// run concurrently (jsonrpc2 multiplexes them over the one connection) so the
// reply waits on the slowest rather than the sum.
func fees(bot *bot, chatID int64) {
    if btcd == nil {
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
        btcPerKB float64
        err      error
    }, len(tiers))
    var wg sync.WaitGroup
    for i, t := range tiers {
        wg.Add(1)
        go func(i, blocks int) {
            defer wg.Done()
            results[i].btcPerKB, results[i].err = btcd.estimateFee(ctx, blocks)
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
        if results[i].err != nil || results[i].btcPerKB <= 0 {
            lines = append(lines, fmt.Sprintf("%-*s  n/a", pad, t.label))
            continue
        }
        available = true
        var rate = strings.TrimSuffix(strconv.FormatFloat(results[i].btcPerKB*1e5, 'f', 1, 64), ".0")
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
    if btcd == nil {
        return
    }
    go func() {
        var sample = func() {
            var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
            var info, err = btcd.getMempoolInfo(ctx)
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

// mempoolSummaryLimit caps how many transactions /mempool will total up — above
// it, the totals (which need the whole verbose mempool plus a fetch per tx) would
// take too long, so the reply degrades to just size and count. A package var so
// it's tunable/testable.
var mempoolSummaryLimit int64 = 20000

// mempoolCmd replies with the current mempool size and transaction count, plus —
// when the mempool is small enough to total up in reasonable time — the summed
// output amount and summed fees of every mempool transaction, in sats and USD.
func mempoolCmd(bot *bot, chatID int64) {
    if btcd == nil {
        send(bot, chatID, "Bitcoin node connection is not configured.")
        return
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    var info, err = btcd.getMempoolInfo(ctx)
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
    case info.Size > mempoolSummaryLimit:
        pairs = append(pairs, [2]string{"Totals", "skipped (mempool too large)"})
    default:
        if amount, fee, ok := mempoolTotals(ctx); ok {
            pairs = append(pairs,
                [2]string{"Total amount", btcAmount(amount)},
                [2]string{"Total fees", btcAmount(fee)},
            )
        } else {
            pairs = append(pairs, [2]string{"Totals", "unavailable"})
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
    var mp, err = btcd.rawMempoolVerbose(ctx)
    if err != nil {
        return 0, 0, false
    }
    var txids = make([]string, 0, len(mp))
    for id, e := range mp {
        fee += e.Fee
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
            var tx, e = btcd.getRawTransaction(ctx, id)
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

func send(bot *bot, chatID int64, text string) {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := bot.send(ctx, chatID, text); err != nil {
        logging.Err("send message: %v", err)
        return
    }
    logging.Info("sent message to chat %d", chatID)
}
