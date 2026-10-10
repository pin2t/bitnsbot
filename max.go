package main

import "context"
import "net/http"
import "net/url"
import "time"
import "bitnsbot/app"
import "bitnsbot/logging"
import "bitnsbot/max"

// maxMessenger is the MAX client as a messenger, which is what every command
// handler and the watch notifier reply through. The client lives in package
// max; the messenger's methods are main's own, so they are here.
type maxMessenger struct {
    bot *max.Bot
}

// newMaxMessenger is the MAX client for token, on the real Bot API. main.go
// cannot build it itself: importing package max there would shadow the builtin
// max it uses.
func newMaxMessenger(token string) maxMessenger { return maxMessenger{max.New(token, max.BaseURL)} }

func (m maxMessenger) platform() string { return "max" }

// sendWithButtons sends the Telegram reply's text with its ids as callback
// buttons, laid out as Telegram's are (buttonRows), each carrying the full id
// as its payload so a tap comes back as a message_callback that runs /info.
func (m maxMessenger) sendWithButtons(ctx context.Context, chat int64, text string, ids []string) error {
    var rows [][]max.Button
    for _, row := range buttonRows(ids) {
        var buttons []max.Button
        for _, btn := range row { buttons = append(buttons, max.Button{Text: btn["text"], Payload: btn["callback_data"]}) }
        rows = append(rows, buttons)
    }
    return m.bot.Send(ctx, chat, text, rows)
}

// maxHandlers runs MAX's updates through what Telegram's go through, so every
// command works there too: a message is dispatched, a tapped id button runs
// /info on it, and a start is answered with the start message. The chat's
// language is set from the update's locale first, since every reply is
// translated through it.
func maxHandlers(mb maxMessenger) max.Handlers {
    return max.Handlers{
        Message: func(chat int64, text, lang string) {
            if lang != "" { SetChatLanguage(chat, lang) }
            dispatch(mb, chat, text)
        },
        Callback: func(chat int64, payload, lang string) {
            if lang != "" { SetChatLanguage(chat, lang) }
            logging.Info("MAX callback %q from chat %d", short(payload), chat)
            info(mb, chat, payload)
        },
        Started: func(chat int64, lang string) {
            if lang != "" { SetChatLanguage(chat, lang) }
            start(mb, chat)
        },
    }
}

// startMax serves MAX's webhook on -max-listen, at the path of -max-webhook-url,
// subscribes that URL when it is set, and starts the MAX Mini App on
// -max-app-listen. A failed subscription is logged rather than fatal: the bot
// still runs on Telegram, and a restart retries it.
//
// The Mini App shows what the Telegram one does, so it hears of the same
// changes the moment that one does.
func startMax(mb maxMessenger) {
    var hookURL, err = url.Parse(*maxWebhookURL)
    if err != nil {
        logging.Fatal("-max-webhook-url: %v", err)
    }
    var hookPath = hookURL.Path
    if hookPath == "" { hookPath = "/" }
    var mux = http.NewServeMux()
    mux.HandleFunc(hookPath, max.Webhook(*maxSecret, maxHandlers(mb)))
    maxSrv = &http.Server{Addr: *maxListen, Handler: mux}
    go func() {
        logging.Status("MAX webhook listening on %s%s", *maxListen, hookPath)
        if err := maxSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            logging.Fatal("MAX webhook listening: %v", err)
        }
    }()
    if *maxWebhookURL != "" {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var serr = mb.bot.Subscribe(ctx, *maxWebhookURL, *maxSecret)
        cancel()
        if serr != nil {
            logging.Err("MAX subscribe: %v", serr)
        } else {
            logging.Status("MAX webhook subscribed at %s", *maxWebhookURL)
        }
    }
    if *maxAppListen != "" {
        app.Listen(max.Notify)
        maxAppSrv = max.Start(*maxAppListen, *maxToken, appOptions(mb))
    }
}
