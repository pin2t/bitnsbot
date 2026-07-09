package main

import "context"
import "crypto/subtle"
import "encoding/hex"
import "encoding/json"
import "flag"
import "fmt"
import "log"
import "net/http"
import "os"
import "strconv"
import "strings"
import "sync"
import "time"

import "github.com/sourcegraph/jsonrpc2"

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

type btcdNotificationLogger struct{}

func (btcdNotificationLogger) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
    var params = "null"
    if req.Params != nil {
        params = string(*req.Params)
    }
    log.Printf("btcd notification: %s %s", req.Method, params)
}

func main() {
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
        flag.PrintDefaults()
    }
    flag.Parse()
    var token = os.Getenv("TELEGRAM_BOT_TOKEN")
    if token == "" {
        log.Fatal("TELEGRAM_BOT_TOKEN environment variable must be set")
    }
    var bot = newBot(token, *apiBaseURL)
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
        }, btcdNotificationLogger{})
        btcdCancel()
        if err != nil {
            log.Fatal("dial btcd: ", err)
        }
        defer btcd.close()
        fmt.Println("Connected to btcd at", *btcdURL)
    }
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
        sendReply(bot, msg.Chat.ID, "Hello! I'm bitnsbot. I'll notify you about Bitcoin network events.")
    case "/info":
        info(bot, msg.Chat.ID, arg)
    case "/watch":
        watch(bot, msg.Chat.ID, arg)
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

func info(bot *bot, chatID int64, arg string) {
    if arg == "" {
        pendingInfoMu.Lock()
        pendingInfoChats[chatID] = true
        pendingInfoMu.Unlock()
        sendReply(bot, chatID, "Please send the info text in a separate message.")
        return
    }
    pendingInfoMu.Lock()
    delete(pendingInfoChats, chatID)
    pendingInfoMu.Unlock()
    if btcd == nil {
        sendReply(bot, chatID, "Bitcoin node connection is not configured.")
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
        sendReply(bot, chatID, "Couldn't find transaction "+txid+".")
        return
    }
    var total float64
    for _, vout := range tx.Vout {
        total += vout.Value
    }
    if tx.Confirmations == 0 {
        sendReply(bot, chatID, fmt.Sprintf("Transaction %s\nStatus: unconfirmed (in mempool)\nAmount: %.8f BTC", tx.Txid, total))
        return
    }
    sendReply(bot, chatID, fmt.Sprintf(
        "Transaction %s\nStatus: confirmed (%d confirmations)\nBlock: %s\nTime: %s\nAmount: %.8f BTC",
        tx.Txid, tx.Confirmations, tx.BlockHash, when(tx.Time), total,
    ))
}

func block(ctx context.Context, bot *bot, chatID int64, height int64) {
    var hash, err = btcd.getBlockHash(ctx, height)
    if err != nil {
        sendReply(bot, chatID, fmt.Sprintf("Couldn't find block %d.", height))
        return
    }
    var header, headerErr = btcd.getBlockHeader(ctx, hash)
    if headerErr != nil {
        log.Println("get block header:", headerErr)
        sendReply(bot, chatID, "Sorry, something went wrong fetching that block.")
        return
    }
    sendReply(bot, chatID, fmt.Sprintf(
        "Block #%d\nHash: %s\nTime: %s\nConfirmations: %d\nDifficulty: %.2f",
        header.Height, header.Hash, when(header.Time), header.Confirmations, header.Difficulty,
    ))
}

func address(ctx context.Context, bot *bot, chatID int64, addr string) {
    var addrInfo, err = btcd.validateAddress(ctx, addr)
    if err != nil {
        log.Println("validate address:", err)
        sendReply(bot, chatID, "Sorry, something went wrong looking up that address.")
        return
    }
    if !addrInfo.IsValid {
        sendReply(bot, chatID, addr+" doesn't look like a valid Bitcoin address.")
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
    sendReply(bot, chatID, fmt.Sprintf("Address %s\nType: %s\nRecent activity: %s", addr, addrType, activity))
}

var pendingWatchMu sync.Mutex
var pendingWatchChats = make(map[int64]bool)

func watch(bot *bot, chatID int64, arg string) {
    if arg == "" {
        pendingWatchMu.Lock()
        pendingWatchChats[chatID] = true
        pendingWatchMu.Unlock()
        sendReply(bot, chatID, "Please send what you'd like to watch in a separate message.")
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
        sendReply(bot, chatID, "Sorry, something went wrong saving that watch.")
        return
    }
    sendReply(bot, chatID, "Watching "+string(typ)+": "+arg)
}

func sendReply(bot *bot, chatID int64, text string) {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := bot.send(ctx, chatID, text); err != nil {
        log.Println("send message:", err)
    }
}
