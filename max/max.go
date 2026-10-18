// Package max is everything the bot does on the MAX messenger: the Bot API
// client, the webhook that receives updates, and the MAX Mini App — the same
// four tabs as the Telegram one, drawn with MAX UI and React instead of HTMX.
//
// Start is the one way in. Running a command is package main's business, so
// the webhook hands each update to the handlers main passes in Options, the
// Mini App reads its chain data through the same app.Options functions the
// Telegram Mini App does, and main replies through Send. The package holds the
// process's one Bot API connection in package variables, the way core holds
// its one node.
package max

import "context"
import "errors"
import "fmt"
import "net"
import "net/http"
import "net/url"
import "regexp"
import "strings"
import "sync"
import "time"
import "bitnsbot/app"
import "bitnsbot/logging"

// Platform is what a watch made on MAX is filed under, and what keeps a MAX
// chat apart from a Telegram chat that happens to share its number.
const Platform = "max"

// apiURL is the MAX Bot API. The docs name platform-api2 in place of the older
// platform-api.
const apiURL = "https://platform-api2.max.ru"

// Options is what Start needs. The handlers take the platform first, so main
// hands MAX and Telegram the very same functions; name is who sent the update
// and lang its user_locale reduced to a language, empty when MAX sent none.
type Options struct {
    // Token is the bot token. It travels in the Authorization header — MAX no
    // longer accepts it as a query parameter — so no URL carries a secret. The
    // Mini App verifies its launch data with it too.
    Token string
    // APIBaseURL is the Bot API called, the real one when empty.
    APIBaseURL string
    // Listen is the address the webhook server binds to; empty starts none.
    Listen string
    // WebhookURL is subscribed with MAX at start when set, and its path is the
    // one the webhook server accepts updates on — "/" when it has none.
    WebhookURL string
    // Secret, when set, is passed to MAX on subscribing and checked against
    // X-Max-Bot-Api-Secret on every update.
    Secret string
    // AppListen is the address the Mini App binds to; empty starts none.
    AppListen string
    // App is the chain data the Mini App renders, asked in Russian.
    App app.Options
    // Message is a message somebody typed: a command, or the argument a bare
    // command asked for. body is the whole update, as it arrived; every handler
    // gets it.
    Message func(platform string, chat int64, name, text, lang string, body []byte)
    // Callback is a tapped id button, payload the full id it carried.
    Callback func(platform string, chat int64, name, payload, lang string, body []byte)
    // Started is the bot being started or added to a chat.
    Started func(platform string, chat int64, name, lang string, body []byte)
}

// mu guards token, baseURL, hookSrv and appSrv: Start sets them while a
// notifier left over from an earlier start, in the tests, may still be sending.
var mu sync.Mutex

// token authenticates every Bot API call; empty until Start. It travels in the
// Authorization header, so no URL carries it.
var token string

// baseURL is the Bot API called.
var baseURL string

// hookSrv and appSrv are the webhook server and the Mini App Start started,
// nil for one it did not.
var hookSrv *http.Server
var appSrv *http.Server

var httpClient = &http.Client{Timeout: 15 * time.Second}

// preBlock is a <pre> block of a message, and padding the spaces its lines are
// aligned with — see Send.
var preBlock = regexp.MustCompile(`(?s)<pre>(.*?)</pre>`)

var padding = regexp.MustCompile(` {2,}`)

// Start sets the token and the API every call goes to, serves the webhook on
// Listen, subscribes WebhookURL, and starts the Mini App on AppListen. A failed
// subscription is logged rather than returned: the bot still runs on Telegram,
// and a restart retries it.
//
// The Mini App shows what the Telegram one does, so it hears of the same
// changes the moment that one does.
func Start(opt Options) error {
    var base = strings.TrimRight(opt.APIBaseURL, "/")
    if base == "" { base = apiURL }
    mu.Lock()
    token, baseURL = opt.Token, base
    mu.Unlock()
    if opt.Listen != "" {
        var hookURL, err = url.Parse(opt.WebhookURL)
        if err != nil { return fmt.Errorf("webhook URL: %w", err) }
        var path = hookURL.Path
        if path == "" { path = "/" }
        var ln, lerr = net.Listen("tcp", opt.Listen)
        if lerr != nil { return lerr }
        var mux = http.NewServeMux()
        mux.HandleFunc(path, webhook(opt))
        var srv = &http.Server{Handler: mux}
        mu.Lock()
        hookSrv = srv
        mu.Unlock()
        go func() {
            logging.Status("MAX webhook listening on %s%s", opt.Listen, path)
            if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
                logging.Fatal("MAX webhook listening: %v", err)
            }
        }()
    }
    if opt.WebhookURL != "" {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var err = subscribe(ctx, opt.WebhookURL, opt.Secret)
        cancel()
        if err != nil {
            logging.Err("MAX subscribe: %v", err)
        } else {
            logging.Status("MAX webhook subscribed at %s", opt.WebhookURL)
        }
    }
    if opt.AppListen != "" {
        app.Listen(notify)
        var srv = startApp(opt.AppListen, opt.Token, opt.App)
        mu.Lock()
        appSrv = srv
        mu.Unlock()
    }
    return nil
}

// Shutdown drains the webhook server and then the Mini App, waiting for the
// requests in flight, so a caller can close what they use once it returns. A
// server Start did not start is skipped.
func Shutdown(ctx context.Context) error {
    mu.Lock()
    var hook, mini = hookSrv, appSrv
    hookSrv, appSrv = nil, nil
    mu.Unlock()
    var errs []error
    if hook != nil {
        if err := hook.Shutdown(ctx); err != nil { errs = append(errs, fmt.Errorf("webhook: %w", err)) }
    }
    if mini != nil {
        if err := mini.Shutdown(ctx); err != nil { errs = append(errs, fmt.Errorf("mini app: %w", err)) }
    }
    return errors.Join(errs...)
}

// Send posts an HTML-formatted message to a chat. The text is the Telegram one,
// MAX's html format understanding the same <b>, <a> and <code>, with one
// change: the field blocks are not <pre>. Telegram's are, so their labels pad
// out to one column in monospace; MAX shows the block as plain text instead, a
// line reading "Размер: 31.5 МБ" rather than with the label's padding, so each
// run of spaces in a <pre> block is made one and the tags are dropped. Each id
// becomes a callback button, two to a row as Telegram lays them out, labelled
// with the shortened id as the text shows it and carrying the full id as its
// payload, so a tap comes back as a message_callback that runs /info. A message
// with no ids carries no keyboard at all. Choosing which ids get a button is the
// caller's.
func Send(ctx context.Context, chat int64, text string, ids []string) error {
    text = preBlock.ReplaceAllStringFunc(text, func(block string) string {
        var lines = strings.Split(preBlock.FindStringSubmatch(block)[1], "\n")
        for i, line := range lines { lines[i] = strings.TrimRight(padding.ReplaceAllString(line, " "), " ") }
        return strings.Join(lines, "\n")
    })
    var path = "/messages?" + url.Values{"chat_id": {fmt.Sprint(chat)}}.Encode()
    var payload = map[string]any{"text": text, "format": "html", "disable_link_preview": true}
    var keyboard [][]map[string]string
    for i, id := range ids {
        var btn = map[string]string{"type": "callback", "text": short(id), "payload": id}
        if i%2 == 0 {
            keyboard = append(keyboard, []map[string]string{btn})
        } else {
            keyboard[len(keyboard)-1] = append(keyboard[len(keyboard)-1], btn)
        }
    }
    if len(keyboard) > 0 {
        payload["attachments"] = []any{map[string]any{"type": "inline_keyboard", "payload": map[string]any{"buttons": keyboard}}}
    }
    return call(ctx, http.MethodPost, path, payload, nil)
}
