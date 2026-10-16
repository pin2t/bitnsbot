// Package max is everything the bot does on the MAX messenger: the Bot API
// client, the webhook that receives updates, and the MAX Mini App — the same
// four tabs as the Telegram one, drawn with MAX UI and React instead of HTMX.
//
// Start is the one way in. Running a command is package main's business, so
// the webhook hands each update to the handlers main passes in Options, the
// Mini App reads its chain data through the same app.Options functions the
// Telegram Mini App does, and main replies through Send. The package holds the
// process's one client, the way core holds its one node.
package max

import "context"
import "errors"
import "fmt"
import "net"
import "net/http"
import "net/url"
import "sync"
import "sync/atomic"
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
    // command asked for.
    Message func(platform string, chat int64, name, text, lang string)
    // Callback is a tapped id button, payload the full id it carried.
    Callback func(platform string, chat int64, name, payload, lang string)
    // Started is the bot being started or added to a chat.
    Started func(platform string, chat int64, name, lang string)
}

// current is the client Send posts through, swapped whole by Start so a send
// already in flight finishes on the client it began with.
var current atomic.Pointer[client]

var serversMu sync.Mutex
var hookSrv *http.Server
var appSrv *http.Server

// Start makes the client every Send uses, serves the webhook on Listen,
// subscribes WebhookURL, and starts the Mini App on AppListen. A failed
// subscription is logged rather than returned: the bot still runs on Telegram,
// and a restart retries it.
//
// The Mini App shows what the Telegram one does, so it hears of the same
// changes the moment that one does.
func Start(opt Options) error {
    var base = opt.APIBaseURL
    if base == "" { base = apiURL }
    var c = newClient(opt.Token, base)
    current.Store(c)
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
        serversMu.Lock()
        hookSrv = srv
        serversMu.Unlock()
        go func() {
            logging.Status("MAX webhook listening on %s%s", opt.Listen, path)
            if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
                logging.Fatal("MAX webhook listening: %v", err)
            }
        }()
    }
    if opt.WebhookURL != "" {
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var err = c.subscribe(ctx, opt.WebhookURL, opt.Secret)
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
        serversMu.Lock()
        appSrv = srv
        serversMu.Unlock()
    }
    return nil
}

// Shutdown drains the webhook server and then the Mini App, waiting for the
// requests in flight, so a caller can close what they use once it returns. A
// server Start did not start is skipped.
func Shutdown(ctx context.Context) error {
    serversMu.Lock()
    var hook, page = hookSrv, appSrv
    hookSrv, appSrv = nil, nil
    serversMu.Unlock()
    var errs []error
    if hook != nil {
        if err := hook.Shutdown(ctx); err != nil { errs = append(errs, fmt.Errorf("webhook: %w", err)) }
    }
    if page != nil {
        if err := page.Shutdown(ctx); err != nil { errs = append(errs, fmt.Errorf("mini app: %w", err)) }
    }
    return errors.Join(errs...)
}

// Send posts an HTML-formatted message to a chat. The text is the Telegram one:
// MAX's html format understands the same <b>, <a>, <code> and <pre>. Each id
// becomes a callback button, two to a row as Telegram lays them out, labelled
// with the shortened id as the text shows it and carrying the full id as its
// payload, so a tap comes back as a message_callback that runs /info. A message
// with no ids carries no keyboard at all. Choosing which ids get a button is the
// caller's.
func Send(ctx context.Context, chat int64, text string, ids []string) error {
    var c = current.Load()
    if c == nil { return errors.New("MAX is not started") }
    return c.send(ctx, chat, text, ids)
}
