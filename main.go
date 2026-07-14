package main

import "context"
import "crypto/subtle"
import "encoding/hex"
import "encoding/json"
import "flag"
import "fmt"
import "html"
import "math"
import "net/http"
import "os"
import "os/signal"
import "slices"
import "strconv"
import "strings"
import "sync"
import "syscall"
import "time"

import "github.com/sourcegraph/jsonrpc2"

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

var btcd *btcdClient

type notification struct {
    txid     string
    received map[string]float64
}

type notifyKey struct {
    chat int64
    typ  watchType
    id   string
}

type notifyChans struct {
    ch   chan notification
    stop chan struct{}
}

var notifyMu sync.Mutex
var notifies = make(map[notifyKey]notifyChans)

func startNotifyChat(b *bot, chatID int64, typ watchType, watchID string) {
    var ch = make(chan notification)
    var stop = make(chan struct{})
    notifyMu.Lock()
    notifies[notifyKey{chatID, typ, watchID}] = notifyChans{ch, stop}
    notifyMu.Unlock()
    go func(b *bot, chatID int64, typ watchType, watchID string, ch <-chan notification, stop chan struct{}) {
        for {
            select {
            case <-stop:
                return
            case n := <-ch:
                if typ == watchTypeAddress {
                    if amount, ok := n.received[watchID]; ok {
                        send(b, chatID, fmt.Sprintf(
                            "🔔 New transaction on watched address %s\n\n<pre>Tx:     %s\nAmount: %s satoshi</pre>",
                            short(watchID), short(n.txid), satoshi(amount),
                        ))
                    }
                } else if n.txid == watchID {
                    send(b, chatID, "🔔 Watched transaction "+short(watchID)+" was accepted to the mempool.")
                }
            }
        }
    }(b, chatID, typ, watchID, ch, stop)
}

func stopNotifyChat(chat int64, typ watchType, id string) {
    notifyMu.Lock()
    defer notifyMu.Unlock()
    var key = notifyKey{chat, typ, id}
    if c, found := notifies[key]; found {
        close(c.stop)
        delete(notifies, key)
    }
}

func notifyAddresses() (res []string) {
    notifyMu.Lock()
    defer notifyMu.Unlock()
    res = make([]string, 0, len(notifies))
    for k, _ := range notifies {
        if k.typ == watchTypeAddress && !slices.Contains(res, k.id) {
            res = append(res, k.id)
        }
    }
    return res
}

// notifier handles btcd's relevanttxaccepted notifications. Rather than
// wrapping it in jsonrpc2.AsyncHandler, Handle spawns the work itself: broadcast
// calls back into btcd (decoderawtransaction) which would deadlock the
// connection's single read loop if run inside a synchronous Handle.
type notifier struct{}

func (notifier) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
    if req.Method != "relevanttxaccepted" || req.Params == nil { return }
    var params []string
    if err := json.Unmarshal(*req.Params, &params); err != nil || len(params) == 0 {
        return
    }
    go broadcast(params[0])
}

func broadcast(txHex string) {
    if btcd == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var tx, err = btcd.decodeRawTransaction(ctx, txHex)
    if err != nil {
        logErr("decode notified tx: %v", err)
        return
    }
    var n = notification{txid: tx.Txid, received: make(map[string]float64)}
    for _, vout := range tx.Vout {
        var addrs = vout.ScriptPubKey.Addresses
        if a := vout.ScriptPubKey.Address; a != "" && !slices.Contains(addrs, a) {
            addrs = append(addrs, a)
        }
        for _, addr := range addrs {
            n.received[addr] += vout.Value
        }
    }
    notifyMu.Lock()
    var chans = make([]chan notification, 0)
    for _, v := range notifies {
        chans = append(chans, v.ch)
    }
    notifyMu.Unlock()
    for _, ch := range chans {
        select {
        case ch <- n:
        case <-time.After(500 * time.Millisecond):
        }
    }
}

// startNotify turns every persisted watchCmd back into a live watcher goroutine
// on startup and, if btcd is connected, loads the watched addresses into btcd's
// transaction filter so notifications resume across restarts.
func startNotify(bot *bot) {
    var records, err = listWatches()
    if err != nil {
        logErr("list watches: %v", err)
        return
    }
    for _, r := range records {
        startNotifyChat(bot, r.ChatID, r.Type, r.WatchID)
    }
    var addrs = notifyAddresses()
    if btcd != nil && len(addrs) > 0 {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        if err := btcd.loadTxFilter(ctx, true, addrs, nil); err != nil {
            logWarn("load tx filter: %v", err)
        }
        cancel()
    }
}

func main() {
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
        flag.PrintDefaults()
    }
    flag.Parse()
    if *configPath != "" {
        if err := applyConfig(*configPath); err != nil {
            logFatal("apply config: %v", err)
        }
    }
    if *botToken == "" {
        logFatal("-bot-token is required")
    }
    var bot = newBot(*botToken, *apiBaseURL)
    var err error
    if err = openDB(*dbPath); err != nil {
        logFatal("open watches database: %v", err)
    }
    if *btcdURL != "" {
        var btcdCtx, btcdCancel = context.WithTimeout(context.Background(), 15*time.Second)
        btcd, err = dialBtcd(btcdCtx, btcdConfig{
            url:         *btcdURL,
            user:        *btcdUser,
            pass:        *btcdPass,
            certFile:    *btcdCert,
            insecureTLS: *btcdInsecureTLS,
        }, notifier{})
        btcdCancel()
        if err != nil {
            logFatal("dial btcd: %v", err)
        }
        logStatus("connected to btcd at %s", *btcdURL)
    }
    startNotify(bot)
    if *registerHook {
        if *webhookURL == "" {
            logFatal("-webhook-url is required when -register-webhook=true")
        }
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var err = bot.setWebhook(ctx, *webhookURL, *secretToken)
        cancel()
        if err != nil {
            logFatal("set webhook: %v", err)
        }
        logStatus("webhook registered at %s", *webhookURL)
    }
    http.HandleFunc(*webhookPath, webhookHandler(bot))
    var srv = &http.Server{Addr: *listenAddr}
    var ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    go func() {
        logStatus("listening on %s", *listenAddr)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            logFatal("error listening: %v", err)
        }
    }()
    <-ctx.Done()
    stop()
    shutdown(srv)
}

func shutdown(srv *http.Server) {
    logStatus("shutting down")
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if err := srv.Shutdown(ctx); err != nil {
        logErr("webhook server shutdown: %v", err)
    }
    if btcd != nil {
        if err := btcd.close(); err != nil {
            logErr("close btcd: %v", err)
        }
    }
    stopNotify()
    if err := closeDB(); err != nil {
        logErr("close watches database: %v", err)
    }
}

func stopNotify() {
    notifyMu.Lock()
    defer notifyMu.Unlock()
    for _, v := range notifies {
        close(v.stop)
    }
    notifies = make(map[notifyKey]notifyChans)
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
            logErr("decode update: %v", err)
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }
        update(bot, u)
        w.WriteHeader(http.StatusOK)
    }
}

var pendingInfoMu sync.Mutex
var pendingInfoChats = make(map[int64]bool)

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
        watches(bot, msg.Chat.ID)
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
    logInfo("message from %s (chat %d): %s", from, msg.Chat.ID, msg.Text)
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

func ago(n int, unit string) string {
    if n == 1 { return "1 " + unit + " ago" }
    return fmt.Sprintf("%d %ss ago", n, unit)
}

func when(unix int64) string {
    var t = time.Unix(unix, 0)
    if t.After(time.Now().AddDate(0, -3, 0)) {
        var since = time.Since(t)
        switch {
        case since < time.Minute:
            return "just now"
        case since < time.Hour:
            return ago(int(since.Minutes()), "minute")
        case since < 24*time.Hour:
            return ago(int(since.Hours()), "hour")
        case since < 31*24*time.Hour:
            return ago(int(since.Hours()/24), "day")
        default:
            return ago(int(since.Hours()/24/30), "month")
        }
    }
    return strings.ToLower(t.UTC().Format("2 January 2006 15:04"))
}

func short(s string) string {
    if len(s) <= 15 { return s }
    return s[:6] + "..." + s[len(s)-6:]
}

func satoshi(btc float64) string {
    var s = strconv.FormatInt(int64(math.Round(btc*1e8)), 10)
    for i := len(s) - 3; i > 0; i -= 3 {
        s = s[:i] + " " + s[i:]
    }
    return s
}

func start(bot *bot, chatID int64) {
    send(bot, chatID, strings.Join([]string{
        "Hi! I'm bitnsbot — I keep an eye on the Bitcoin network for you.",
        "",
        "• <b>/info</b> — look up a transaction, block, or address",
        "• <b>/watch</b> — get notified when an address receives a transaction",
        "• <b>/unwatch</b> — stop watching an address or transaction",
        "• <b>/watches</b> — list what you're currently watching",
        "• <b>/start</b> — show this message",
    }, "\n"))
}

func info(bot *bot, chatID int64, arg string) {
    if arg == "" {
        pendingInfoMu.Lock()
        pendingInfoChats[chatID] = true
        pendingInfoMu.Unlock()
        send(bot, chatID, "Please send Bitcoin address or transaction or block number")
        return
    }
    pendingInfoMu.Lock()
    delete(pendingInfoChats, chatID)
    pendingInfoMu.Unlock()
    if btcd == nil {
        send(bot, chatID, "Bitcoin node connection is not configured.")
        return
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if isTxid(arg) {
        transaction(ctx, bot, chatID, arg)
        return
    }
    if height, err := strconv.ParseInt(arg, 10, 64); err == nil && height >= 0 {
        block(ctx, bot, chatID, height)
        return
    }
    address(ctx, bot, chatID, arg)
}

func transaction(ctx context.Context, bot *bot, chatID int64, txid string) {
    var tx, err = btcd.getRawTransaction(ctx, txid)
    if err != nil {
        send(bot, chatID, "Couldn't find transaction "+short(txid)+".")
        return
    }
    var total float64
    for _, vout := range tx.Vout {
        total += vout.Value
    }
    var amount = satoshi(total)
    if tx.Confirmations == 0 {
        send(bot, chatID, fmt.Sprintf("Transaction %s\n\n<pre>Status: unconfirmed (in mempool)\nAmount: %s satoshi</pre>", short(tx.Txid), amount))
        return
    }
    send(bot, chatID, fmt.Sprintf(
        "Transaction %s\n\n<pre>Status: confirmed (%d confirmations)\nBlock:  %s\nTime:   %s\nAmount: %s satoshi</pre>",
        short(tx.Txid), tx.Confirmations, short(tx.BlockHash), when(tx.Time), amount,
    ))
}

func block(ctx context.Context, bot *bot, chatID int64, height int64) {
    var hash, err = btcd.getBlockHash(ctx, height)
    if err != nil {
        send(bot, chatID, fmt.Sprintf("Couldn't find block %d.", height))
        return
    }
    var header, headerErr = btcd.getBlockHeader(ctx, hash)
    if headerErr != nil {
        logErr("get block header: %v", headerErr)
        send(bot, chatID, "Sorry, something went wrong fetching that block.")
        return
    }
    var difficulty = header.Difficulty
    var unit = ""
    for _, u := range []string{" k", " M", " G", " T", " P", " E"} {
        if difficulty < 1000 { break }
        difficulty /= 1000
        unit = u
    }
    var diff = strings.TrimRight(strings.TrimRight(strconv.FormatFloat(difficulty, 'f', 2, 64), "0"), ".") + unit
    send(bot, chatID, fmt.Sprintf(
        "Block #%d\n\n<pre>Hash:          %s\nTime:          %s\nConfirmations: %d\nDifficulty:    %s</pre>",
        header.Height, short(header.Hash), when(header.Time), header.Confirmations, diff,
    ))
}

func address(ctx context.Context, bot *bot, chatID int64, addr string) {
    var addrInfo, err = btcd.validateAddress(ctx, addr)
    if err != nil {
        logErr("validate address: %v", err)
        send(bot, chatID, "Sorry, something went wrong looking up that address.")
        return
    }
    if !addrInfo.IsValid {
        send(bot, chatID, html.EscapeString(addr)+" doesn't look like a valid Bitcoin address.")
        return
    }
    var addrType = "standard (P2PKH)"
    if addrInfo.IsWitness {
        addrType = "segwit (bech32)"
    } else if addrInfo.IsScript {
        addrType = "script hash (P2SH)"
    }
    var activity = "unavailable (address index not enabled)"
    if txs, txErr := btcd.searchRawTransactions(ctx, addr, 10); txErr == nil {
        activity = fmt.Sprintf("%d transaction(s) found", len(txs))
    }
    send(bot, chatID, fmt.Sprintf("Address %s\n\n<pre>Type:            %s\nRecent activity: %s</pre>", short(addr), addrType, activity))
}

var pendingWatchMu sync.Mutex
var pendingWatchChats = make(map[int64]bool)

func watchCmd(bot *bot, chatID int64, arg string) {
    if arg == "" {
        pendingWatchMu.Lock()
        pendingWatchChats[chatID] = true
        pendingWatchMu.Unlock()
        send(bot, chatID, "Please send what you'd like to watch in a separate message.")
        return
    }
    pendingWatchMu.Lock()
    delete(pendingWatchChats, chatID)
    pendingWatchMu.Unlock()
    var typ = watchTypeAddress
    if isTxid(arg) {
        typ = watchTypeTransaction
    }
    if err := addWatch(chatID, typ, arg); err != nil {
        logErr("add watch: %v", err)
        send(bot, chatID, "Sorry, something went wrong saving that watch.")
        return
    }
    startNotifyChat(bot, chatID, typ, arg)
    logInfo("added %s subscription %s for chat %d", typ, arg, chatID)
    if typ == watchTypeAddress && btcd != nil {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        if err := btcd.loadTxFilter(ctx, false, []string{arg}, nil); err != nil {
            logWarn("load tx filter: %v", err)
        }
        cancel()
    }
    send(bot, chatID, "Watching "+string(typ)+": "+html.EscapeString(arg))
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
    var removed, err = removeWatch(chatID, arg)
    if err != nil {
        logErr("remove watch: %v", err)
        send(bot, chatID, "Sorry, something went wrong removing that watch.")
        return
    }
    if removed == 0 {
        send(bot, chatID, "You're not watching "+html.EscapeString(arg)+".")
        return
    }
    var typ = watchTypeAddress
    if isTxid(arg) { typ = watchTypeTransaction }
    stopNotifyChat(chatID, typ, arg)
    logInfo("removed subscription %s for chat %d", arg, chatID)
    if !isTxid(arg) && btcd != nil {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        if err := btcd.loadTxFilter(ctx, true, notifyAddresses(), nil); err != nil {
            logWarn("load tx filter: %v", err)
        }
        cancel()
    }
    send(bot, chatID, "Stopped watching "+html.EscapeString(arg)+".")
}

func watches(bot *bot, chatID int64) {
    var records, err = listWatches()
    if err != nil {
        logErr("list watches: %v", err)
        send(bot, chatID, "Sorry, something went wrong listing your watches.")
        return
    }
    var addresses, transactions []string
    for _, r := range records {
        if r.ChatID != chatID {
            continue
        }
        if r.Type == watchTypeTransaction {
            transactions = append(transactions, r.WatchID)
        } else {
            addresses = append(addresses, r.WatchID)
        }
    }
    if len(addresses) == 0 && len(transactions) == 0 {
        send(bot, chatID, "You're not watching anything yet.")
        return
    }
    var lines = []string{"Your watches:"}
    if len(addresses) > 0 {
        lines = append(lines, "", "Addresses:")
        for _, a := range addresses {
            lines = append(lines, "<code>"+html.EscapeString(a)+"</code>")
        }
    }
    if len(transactions) > 0 {
        lines = append(lines, "", "Transactions:")
        for _, t := range transactions {
            lines = append(lines, "<code>"+html.EscapeString(t)+"</code>")
        }
    }
    send(bot, chatID, strings.Join(lines, "\n"))
}

func send(bot *bot, chatID int64, text string) {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := bot.send(ctx, chatID, text); err != nil {
        logErr("send message: %v", err)
        return
    }
    logInfo("sent message to chat %d", chatID)
}
