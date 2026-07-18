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
    txid         string
    received     map[string]float64
    fee          float64 // BTC
    feeRate      float64 // sat/vB
    confEstimate string  // "~10-20 min" etc; "" if unavailable
    feeOK        bool    // whether fee/feeRate are populated
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

func startNotifyChat(b *bot, chatID int64, typ watchType, watchID, alias string) {
    var ch = make(chan notification)
    var stop = make(chan struct{})
    notifyMu.Lock()
    notifies[notifyKey{chatID, typ, watchID}] = notifyChans{ch, stop}
    notifyMu.Unlock()
    go func(b *bot, chatID int64, typ watchType, watchID, alias string, ch <-chan notification, stop chan struct{}) {
        var label = short(watchID)
        if alias != "" {
            label += " (" + html.EscapeString(alias) + ")"
        }
        for {
            select {
            case <-stop:
                return
            case n := <-ch:
                if typ == watchTypeAddress {
                    if amount, ok := n.received[watchID]; ok {
                        var pairs = [][2]string{
                            {"Tx", short(n.txid)},
                            {"Amount", amountLine(amount, time.Time{}, true)},
                        }
                        if n.feeOK {
                            pairs = append(pairs,
                                [2]string{"Fee", satoshi(n.fee) + " sats"},
                                [2]string{"Fee rate", strings.TrimSuffix(strconv.FormatFloat(n.feeRate, 'f', 1, 64), ".0") + " sat/vB"},
                            )
                            if n.confEstimate != "" {
                                pairs = append(pairs, [2]string{"Confirms", n.confEstimate})
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
                        send(b, chatID, "🔔 New transaction on watched address "+short(watchID)+"\n\n<pre>"+strings.Join(lines, "\n")+"</pre>")
                    }
                } else if n.txid == watchID {
                    send(b, chatID, "🔔 Watched transaction "+label+" was accepted to the mempool.")
                }
            }
        }
    }(b, chatID, typ, watchID, alias, ch, stop)
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
    if req.Params == nil { return }
    switch req.Method {
    case "relevanttxaccepted":
        var params []string
        if err := json.Unmarshal(*req.Params, &params); err == nil && len(params) > 0 {
            go broadcast(params[0])
        }
    case "blockconnected":
        var params []json.RawMessage
        if err := json.Unmarshal(*req.Params, &params); err == nil && len(params) > 0 {
            var hash string
            if json.Unmarshal(params[0], &hash) == nil {
                go cacheBlockHash(hash)
            }
        }
    }
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
    if full, ferr := btcd.getRawTransaction(ctx, tx.Txid); ferr == nil && full.Vsize > 0 {
        if fee, _, ok := txInputs(ctx, full); ok {
            n.fee = fee
            n.feeRate = math.Round(fee*1e8) / float64(full.Vsize)
            n.confEstimate = confEstimate(ctx, n.feeRate)
            n.feeOK = true
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

// confEstimate maps a transaction's fee rate (sat/vB) to a rough confirmation
// window by comparing it to btcd's fee estimates (BTC/kB → sat/vB via ×1e5) for
// the 2- and 6-block targets. Returns "" if the estimator has no data yet.
func confEstimate(ctx context.Context, feeRate float64) string {
    var fast, ferr = btcd.estimateFee(ctx, 2)
    var medium, merr = btcd.estimateFee(ctx, 6)
    if ferr != nil || merr != nil || fast <= 0 || medium <= 0 {
        return ""
    }
    switch {
    case feeRate >= fast*1e5:
        return "~10-20 min"
    case feeRate >= medium*1e5:
        return "~1h"
    default:
        return "2h+"
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
        startNotifyChat(bot, r.ChatID, r.Type, r.WatchID, r.Alias)
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

// reapplyBtcdState re-establishes btcd's stateful subscriptions after a
// reconnect — the block-notification subscription and the transaction filter for
// every watched address. Passed to btcd.supervise as its reconnect callback.
func reapplyBtcdState() {
    if btcd == nil { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if err := btcd.notifyBlocks(ctx); err != nil {
        logWarn("resubscribe to blocks: %v", err)
    }
    var addrs = notifyAddresses()
    if len(addrs) > 0 {
        if err := btcd.loadTxFilter(ctx, true, addrs, nil); err != nil {
            logWarn("reload tx filter: %v", err)
        }
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
    startRatesUpdater()
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
    startBlockCache()
    if btcd != nil {
        btcd.supervise(reapplyBtcdState)
    }
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
    case "/fees":
        fees(bot, msg.Chat.ID)
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

func group(n int64) string {
    var s = strconv.FormatInt(n, 10)
    for i := len(s) - 3; i > 0; i -= 3 {
        s = s[:i] + " " + s[i:]
    }
    return s
}

func satoshi(btc float64) string {
    return group(int64(math.Round(btc * 1e8)))
}

func start(bot *bot, chatID int64) {
    send(bot, chatID, strings.Join([]string{
        "Hi! I'm bitnsbot — I keep an eye on the Bitcoin network for you.",
        "",
        "• <b>/info</b> — look up a transaction, block, or address",
        "• <b>/watch</b> — get notified when an address receives a transaction",
        "• <b>/unwatch</b> — stop watching an address or transaction",
        "• <b>/watches</b> — list what you're currently watching",
        "• <b>/fees</b> — show current network fee estimates",
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
    var coinbase = len(tx.Vin) > 0 && tx.Vin[0].Coinbase != ""
    var fee float64
    var inputs []string
    var feeOK bool
    if !coinbase {
        fee, inputs, feeOK = txInputs(ctx, tx)
    }
    var pairs [][2]string
    var at = time.Time{}
    var current = true
    if tx.Confirmations == 0 {
        pairs = append(pairs, [2]string{"Status", "unconfirmed (in mempool)"})
    } else {
        at, current = time.Unix(tx.Time, 0), false
        pairs = append(pairs,
            [2]string{"Status", fmt.Sprintf("confirmed (%d confirmations)", tx.Confirmations)},
            [2]string{"Confirmed", when(tx.Time)},
            [2]string{"Block", short(tx.BlockHash)},
        )
    }
    pairs = append(pairs, [2]string{"Amount", amountLine(total, at, current)})
    switch {
    case coinbase:
        pairs = append(pairs, [2]string{"Fee", "none (coinbase)"})
    case feeOK:
        pairs = append(pairs, [2]string{"Fee", satoshi(fee) + " sats"})
    default:
        pairs = append(pairs, [2]string{"Fee", "unavailable"})
    }
    pairs = append(pairs, [2]string{"Size", group(int64(tx.Size)) + " bytes"})
    switch {
    case coinbase:
        pairs = append(pairs, [2]string{"Inputs", "coinbase (newly generated)"})
    case feeOK:
        pairs = append(pairs, [2]string{"Inputs", compactAddrs(inputs)})
    default:
        pairs = append(pairs, [2]string{"Inputs", "unavailable"})
    }
    pairs = append(pairs, [2]string{"Outputs", compactAddrs(outputAddrs(tx))})
    var pad int
    for _, p := range pairs {
        if len(p[0])+1 > pad { pad = len(p[0]) + 1 }
    }
    var lines []string
    for _, p := range pairs {
        lines = append(lines, fmt.Sprintf("%-*s %s", pad, p[0]+":", p[1]))
    }
    send(bot, chatID, fmt.Sprintf("Transaction %s\n\n<pre>%s</pre>", short(tx.Txid), strings.Join(lines, "\n")))
}

// txInputs fetches each input's prevout transaction to sum the input values (for
// the fee = inputs − outputs) and collect the spent addresses. btcd's
// getrawtransaction gives inputs only as txid:vout refs, so the prevouts are
// fetched concurrently (bounded); any fetch failure yields ok=false so the reply
// shows the fee/inputs as unavailable rather than wrong.
func txInputs(ctx context.Context, tx *btcdTransaction) (fee float64, addrs []string, ok bool) {
    var ids = map[string]bool{}
    for _, in := range tx.Vin {
        ids[in.Txid] = true
    }
    var prevouts = map[string]*btcdTransaction{}
    var mu sync.Mutex
    var wg sync.WaitGroup
    var sem = make(chan struct{}, 16)
    var fetchErr error
    for id := range ids {
        wg.Add(1)
        sem <- struct{}{}
        go func(id string) {
            defer wg.Done()
            defer func() { <-sem }()
            var p, e = btcd.getRawTransaction(ctx, id)
            mu.Lock()
            if e != nil {
                if fetchErr == nil { fetchErr = e }
            } else {
                prevouts[id] = p
            }
            mu.Unlock()
        }(id)
    }
    wg.Wait()
    if fetchErr != nil {
        return 0, nil, false
    }
    var inSum float64
    for _, vin := range tx.Vin {
        var p = prevouts[vin.Txid]
        if p == nil || int(vin.Vout) >= len(p.Vout) {
            return 0, nil, false
        }
        inSum += p.Vout[vin.Vout].Value
        addrs = append(addrs, addressOf(p.Vout[vin.Vout]))
    }
    var outSum float64
    for _, v := range tx.Vout {
        outSum += v.Value
    }
    fee = inSum - outSum
    if fee < 0 {
        fee = 0
    }
    return fee, addrs, true
}

func outputAddrs(tx *btcdTransaction) []string {
    var addrs []string
    for _, v := range tx.Vout {
        addrs = append(addrs, addressOf(v))
    }
    return addrs
}

func addressOf(v btcdVout) string {
    if v.ScriptPubKey.Address != "" { return v.ScriptPubKey.Address }
    if len(v.ScriptPubKey.Addresses) > 0 { return v.ScriptPubKey.Addresses[0] }
    return "(non-standard)"
}

// compactAddrs joins shortened addresses, showing at most the first three with a
// trailing "..." when there are more.
func compactAddrs(addrs []string) string {
    if len(addrs) == 0 {
        return "none"
    }
    var show, more = addrs, false
    if len(addrs) > 3 {
        show, more = addrs[:3], true
    }
    var parts []string
    for _, a := range show {
        parts = append(parts, short(a))
    }
    var s = strings.Join(parts, ", ")
    if more {
        s += ", ..."
    }
    return s
}

func block(ctx context.Context, bot *bot, chatID int64, height int64) {
    if bi, ok := loadBlock(height); ok {
        send(bot, chatID, formatBlock(bi))
        return
    }
    var hash, err = btcd.getBlockHash(ctx, height)
    if err != nil {
        send(bot, chatID, fmt.Sprintf("Couldn't find block %d.", height))
        return
    }
    var bi, ciErr = computeBlockInfo(ctx, hash)
    if ciErr != nil {
        logErr("compute block %d: %v", height, ciErr)
        send(bot, chatID, "Sorry, something went wrong fetching that block.")
        return
    }
    storeBlock(bi)
    send(bot, chatID, formatBlock(bi))
}

// blockFees computes each non-coinbase transaction's fee (sum of input prevout
// values minus output values) and returns the low/average/high in BTC with the
// count of fee-paying transactions. btcd's getblock omits input values, so every
// referenced prevout transaction is fetched concurrently (bounded) and cached;
// if any fetch fails (e.g. txindex disabled, or a genuinely missing prevout) it
// returns an error so the caller reports fees as unavailable rather than wrong.
func blockFees(ctx context.Context, txs []btcdBlockTx) (low, avg, high float64, count int, err error) {
    var ids = map[string]bool{}
    for _, tx := range txs {
        if len(tx.Vin) > 0 && tx.Vin[0].Coinbase != "" {
            continue
        }
        for _, in := range tx.Vin {
            ids[in.Txid] = true
        }
    }
    if len(ids) == 0 {
        return 0, 0, 0, 0, nil
    }
    var prevouts = map[string]*btcdTransaction{}
    var mu sync.Mutex
    var wg sync.WaitGroup
    var sem = make(chan struct{}, 16)
    var fetchErr error
    for id := range ids {
        wg.Add(1)
        sem <- struct{}{}
        go func(id string) {
            defer wg.Done()
            defer func() { <-sem }()
            var tx, e = btcd.getRawTransaction(ctx, id)
            mu.Lock()
            if e != nil {
                if fetchErr == nil { fetchErr = e }
            } else {
                prevouts[id] = tx
            }
            mu.Unlock()
        }(id)
    }
    wg.Wait()
    if fetchErr != nil {
        return 0, 0, 0, 0, fetchErr
    }
    var total float64
    for _, tx := range txs {
        if len(tx.Vin) > 0 && tx.Vin[0].Coinbase != "" {
            continue
        }
        var in, out float64
        for _, vin := range tx.Vin {
            var p = prevouts[vin.Txid]
            if p == nil || int(vin.Vout) >= len(p.Vout) {
                return 0, 0, 0, 0, fmt.Errorf("missing prevout %s:%d", vin.Txid, vin.Vout)
            }
            in += p.Vout[vin.Vout].Value
        }
        for _, vout := range tx.Vout {
            out += vout.Value
        }
        var fee = in - out
        if fee < 0 {
            fee = 0
        }
        if count == 0 {
            low, high = fee, fee
        } else {
            if fee < low { low = fee }
            if fee > high { high = fee }
        }
        total += fee
        count++
    }
    if count == 0 {
        return 0, 0, 0, 0, nil
    }
    return low, total / float64(count), high, count, nil
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
    if err := addWatch(chatID, typ, watchID, alias); err != nil {
        logErr("add watch: %v", err)
        send(bot, chatID, "Sorry, something went wrong saving that watch.")
        return
    }
    startNotifyChat(bot, chatID, typ, watchID, alias)
    logInfo("added %s subscription %s for chat %d (alias %q)", typ, watchID, chatID, alias)
    if typ == watchTypeAddress && btcd != nil {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        if err := btcd.loadTxFilter(ctx, false, []string{watchID}, nil); err != nil {
            logWarn("load tx filter: %v", err)
        }
        cancel()
    }
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
        var line = "<code>" + html.EscapeString(r.WatchID) + "</code>"
        if r.Alias != "" {
            line += " (" + html.EscapeString(r.Alias) + ")"
        }
        if r.Type == watchTypeTransaction {
            transactions = append(transactions, line)
        } else {
            addresses = append(addresses, line)
        }
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

func send(bot *bot, chatID int64, text string) {
    var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := bot.send(ctx, chatID, text); err != nil {
        logErr("send message: %v", err)
        return
    }
    logInfo("sent message to chat %d", chatID)
}
