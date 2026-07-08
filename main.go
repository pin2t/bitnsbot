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
import "strings"
import "sync"
import "time"

var listenAddr   = flag.String("listen", ":8080", "listen address")
var webhookPath  = flag.String("webhook-path", "/bot", "path the Bot API server will POST updates to")
var webhookURL   = flag.String("webhook-url", "", "URL the Bot API server should send updates to, e.g. http://localhost:8080/bot")
var apiBaseURL   = flag.String("api-base-url", "http://localhost:8081", "base URL of the local telegram-bot-api server")
var secretToken  = flag.String("secret-token", "", "optional secret checked against the X-Telegram-Bot-Api-Secret-Token header")
var registerHook = flag.Bool("register-webhook", true, "call setWebhook on startup")
var dbPath       = flag.String("db", "watches.db", "path to the bbolt watches database")

var store *watchStore

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
	sendReply(bot, chatID, "Info: "+arg)
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
	if _, err := hex.DecodeString(arg); len(arg) == 64 && err == nil {
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
