package main

import "context"
import "crypto/subtle"
import "encoding/json"
import "flag"
import "fmt"
import "log"
import "net/http"
import "os"
import "strings"
import "sync"
import "time"

var (
	listenAddr   = flag.String("listen", ":8080", "listen address")
	webhookPath  = flag.String("webhook-path", "/bot", "path the Bot API server will POST updates to")
	webhookURL   = flag.String("webhook-url", "", "URL the Bot API server should send updates to, e.g. http://localhost:8080/bot")
	apiBaseURL   = flag.String("api-base-url", "http://localhost:8081", "base URL of the local telegram-bot-api server")
	secretToken  = flag.String("secret-token", "", "optional secret checked against the X-Telegram-Bot-Api-Secret-Token header")
	registerHook = flag.Bool("register-webhook", true, "call setWebhook on startup")
)

var Usage = func() {
	fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	flag.Usage = Usage
	flag.Parse()
	var token = os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable must be set")
	}
	var bot = NewBot(token, *apiBaseURL)
	if *registerHook {
		if *webhookURL == "" {
			log.Fatal("-webhook-url is required when -register-webhook=true")
		}
		var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
		var err = bot.SetWebhook(ctx, SetWebhookOptions{
			URL:         *webhookURL,
			SecretToken: *secretToken,
		})
		cancel()
		if err != nil {
			log.Fatal("set webhook: ", err)
		}
		fmt.Println("Webhook registered at", *webhookURL)
	}
	http.HandleFunc(*webhookPath, webhookHandler(bot))
	fmt.Println("Listening", *listenAddr, "...")
	var err = http.ListenAndServe(*listenAddr, nil)
	if err != nil {
		log.Fatal("error listening: ", err)
	}
}

func webhookHandler(bot *Bot) http.HandlerFunc {
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
		var update Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			log.Println("decode update:", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		handleUpdate(bot, update)
		w.WriteHeader(http.StatusOK)
	}
}

var pendingInfoMu sync.Mutex
var pendingInfoChats = make(map[int64]bool)

func handleUpdate(bot *Bot, update Update) {
	var msg = update.Message
	if msg == nil {
		return
	}
	logMessage(msg)
	var command, arg = parseCommand(msg.Text)
	switch command {
	case "/start":
		sendReply(bot, msg.Chat.ID, "Hello! I'm bitnsbot. I'll notify you about Bitcoin network events.")
	case "/info":
		handleInfo(bot, msg.Chat.ID, arg)
	case "":
		if takePendingInfo(msg.Chat.ID) {
			handleInfo(bot, msg.Chat.ID, msg.Text)
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

// parseCommand splits a message's text into a leading "/command" and the
// rest of the text, or returns an empty command if the message isn't one.
func parseCommand(text string) (command, arg string) {
	var fields = strings.SplitN(strings.TrimSpace(text), " ", 2)
	if !strings.HasPrefix(fields[0], "/") {
		return "", ""
	}
	command = strings.SplitN(fields[0], "@", 2)[0] // drop the "@botname" suffix Telegram appends in groups
	if len(fields) > 1 {
		arg = strings.TrimSpace(fields[1])
	}
	return command, arg
}

func handleInfo(bot *Bot, chatID int64, arg string) {
	if arg == "" {
		setPendingInfo(chatID)
		sendReply(bot, chatID, "Please send the info text in a separate message.")
		return
	}
	clearPendingInfo(chatID)
	sendReply(bot, chatID, "Info: "+arg)
}

func setPendingInfo(chatID int64) {
	pendingInfoMu.Lock()
	pendingInfoChats[chatID] = true
	pendingInfoMu.Unlock()
}

// takePendingInfo reports whether chatID was awaiting an /info argument,
// clearing the pending state if so.
func takePendingInfo(chatID int64) bool {
	pendingInfoMu.Lock()
	defer pendingInfoMu.Unlock()
	if !pendingInfoChats[chatID] {
		return false
	}
	delete(pendingInfoChats, chatID)
	return true
}

func clearPendingInfo(chatID int64) {
	pendingInfoMu.Lock()
	delete(pendingInfoChats, chatID)
	pendingInfoMu.Unlock()
}

func sendReply(bot *Bot, chatID int64, text string) {
	var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bot.SendMessage(ctx, chatID, text); err != nil {
		log.Println("send message:", err)
	}
}
