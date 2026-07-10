package main

import "context"
import "crypto/subtle"
import "encoding/hex"
import "encoding/json"
import "flag"
import "fmt"
import "html"
import "log"
import "math"
import "net/http"
import "os"
import "slices"
import "strconv"
import "strings"
import "sync"
import "time"

import "github.com/sourcegraph/jsonrpc2"

var configPath      = flag.String("config", "", "path to a properties file (name=value lines) with flag values; command-line flags take precedence")
var botToken        = flag.String("tg-bot-token", "", "Telegram bot token authenticating outbound Bot API calls (required)")
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

var store *watchStore
var btcd *btcdClient

var watchersMu sync.Mutex
var watchersByAddr = make(map[string][]int64)

func addWatcher(addr string, chatID int64) {
    watchersMu.Lock()
    defer watchersMu.Unlock()
    if slices.Contains(watchersByAddr[addr], chatID) { return }
    watchersByAddr[addr] = append(watchersByAddr[addr], chatID)
}

func watchersOf(addr string) []int64 {
    watchersMu.Lock()
    defer watchersMu.Unlock()
    return slices.Clone(watchersByAddr[addr])
}

func removeWatcher(addr string, chatID int64) {
    watchersMu.Lock()
    defer watchersMu.Unlock()
    var ids = slices.DeleteFunc(watchersByAddr[addr], func(id int64) bool { return id == chatID })
    if len(ids) == 0 {
        delete(watchersByAddr, addr)
    } else {
        watchersByAddr[addr] = ids
    }
}

func watchedAddresses() []string {
    watchersMu.Lock()
    defer watchersMu.Unlock()
    var addrs = make([]string, 0, len(watchersByAddr))
    for addr := range watchersByAddr {
        addrs = append(addrs, addr)
    }
    return addrs
}

// btcdNotifier turns btcd's relevanttxaccepted notifications (raw mempool
// transactions matching the loaded address filter) into Telegram messages. It
// is wrapped in jsonrpc2.AsyncHandler so it can call back into btcd
// (decoderawtransaction) without deadlocking the connection's read loop.
type btcdNotifier struct {
    bot *bot
}

func (n btcdNotifier) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
    if req.Method != "relevanttxaccepted" || req.Params == nil { return }
    var params []string
    if err := json.Unmarshal(*req.Params, &params); err != nil || len(params) == 0 {
        return
    }
    notifyWatchers(n.bot, params[0])
}

func notifyWatchers(bot *bot, txHex string) {
    if btcd == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var tx, err = btcd.decodeRawTransaction(ctx, txHex)
    if err != nil {
        log.Println("decode notified tx:", err)
        return
    }
    var received = make(map[string]float64)
    for _, vout := range tx.Vout {
        var addrs = vout.ScriptPubKey.Addresses
        if a := vout.ScriptPubKey.Address; a != "" && !slices.Contains(addrs, a) {
            addrs = append(addrs, a)
        }
        for _, addr := range addrs {
            received[addr] += vout.Value
        }
    }
    for addr, amount := range received {
        for _, chatID := range watchersOf(addr) {
            send(bot, chatID, fmt.Sprintf(
                "🔔 New transaction on watched address %s\n\n<pre>Tx:     %s\nAmount: %s satoshi</pre>",
                short(addr), short(tx.Txid), satoshi(amount),
            ))
        }
    }
}

// restoreWatches rebuilds the in-memory address→chats routing map from the
// persisted store on startup and, if btcd is connected, loads every watched
// address into btcd's transaction filter so notifications resume across restarts.
func restoreWatches() {
    var records, err = store.list()
    if err != nil {
        log.Println("list watches:", err)
        return
    }
    var addrs []string
    for _, r := range records {
        if r.Type != watchTypeAddress { continue }
        addWatcher(r.WatchID, r.ChatID)
        if !slices.Contains(addrs, r.WatchID) {
            addrs = append(addrs, r.WatchID)
        }
    }
    if btcd != nil && len(addrs) > 0 {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        if err := btcd.loadTxFilter(ctx, true, addrs, nil); err != nil {
            log.Println("load tx filter:", err)
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
            log.Fatal("apply config: ", err)
        }
    }
    if *botToken == "" {
        log.Fatal("-tg-bot-token is required")
    }
    var bot = newBot(*botToken, *apiBaseURL)
    var err error
    store, err = openWatchStore(*dbPath)
    if err != nil {
        log.Fatal("open watches database: ", err)
    }
    defer store.close()
    if *btcdURL != "" {
        var btcdCtx, btcdCancel = context.WithTimeout(context.Background(), 15*time.Second)
        btcd, err = dialBtcd(btcdCtx, btcdConfig{
            url:         *btcdURL,
            user:        *btcdUser,
            pass:        *btcdPass,
            certFile:    *btcdCert,
            insecureTLS: *btcdInsecureTLS,
        }, jsonrpc2.AsyncHandler(btcdNotifier{bot: bot}))
        btcdCancel()
        if err != nil {
            log.Fatal("dial btcd: ", err)
        }
        defer btcd.close()
        fmt.Println("Connected to btcd at", *btcdURL)
    }
    restoreWatches()
    if *registerHook {
        if *webhookURL == "" {
            log.Fatal("-webhook-url is required when -register-webhook=true")
        }
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var err = bot.setWebhook(ctx, *webhookURL, *secretToken)
        cancel()
        if err != nil {
            log.Fatal("set webhook: ", err)
        }
        fmt.Println("Webhook registered at", *webhookURL)
    }
    http.HandleFunc(*webhookPath, webhookHandler(bot))
    fmt.Println("Listening", *listenAddr, "...")
    err = http.ListenAndServe(*listenAddr, nil)
    if err != nil {
        log.Fatal("error listening: ", err)
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
            log.Println("decode update:", err)
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
        send(bot, msg.Chat.ID, "Hello! I'm bitnsbot. I'll notify you about Bitcoin network events.")
    case "/info":
        info(bot, msg.Chat.ID, arg)
    case "/watch":
        watch(bot, msg.Chat.ID, arg)
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
            watch(bot, msg.Chat.ID, msg.Text)
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
    log.Printf("message from %s (chat %d): %s", from, msg.Chat.ID, msg.Text)
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
        log.Println("get block header:", headerErr)
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
        log.Println("validate address:", err)
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

func watch(bot *bot, chatID int64, arg string) {
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
    if err := store.add(chatID, typ, arg); err != nil {
        log.Println("add watch:", err)
        send(bot, chatID, "Sorry, something went wrong saving that watch.")
        return
    }
    if typ == watchTypeAddress {
        addWatcher(arg, chatID)
        if btcd != nil {
            var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
            if err := btcd.loadTxFilter(ctx, false, []string{arg}, nil); err != nil {
                log.Println("load tx filter:", err)
            }
            cancel()
        }
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
    var removed, err = store.remove(chatID, arg)
    if err != nil {
        log.Println("remove watch:", err)
        send(bot, chatID, "Sorry, something went wrong removing that watch.")
        return
    }
    if removed == 0 {
        send(bot, chatID, "You're not watching "+html.EscapeString(arg)+".")
        return
    }
    if !isTxid(arg) {
        removeWatcher(arg, chatID)
        if btcd != nil {
            var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
            if err := btcd.loadTxFilter(ctx, true, watchedAddresses(), nil); err != nil {
                log.Println("load tx filter:", err)
            }
            cancel()
        }
    }
    send(bot, chatID, "Stopped watching "+html.EscapeString(arg)+".")
}

func watches(bot *bot, chatID int64) {
    var records, err = store.list()
    if err != nil {
        log.Println("list watches:", err)
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
        log.Println("send message:", err)
    }
}
