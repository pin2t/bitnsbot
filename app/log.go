package app

import "net"
import "net/http"
import "strings"
import "time"
import "bitnsbot/logging"

// logWriter is the ResponseWriter Log hands a Mini App's handler, so the handler
// can say its answer came from a cache (Hit) — which nothing outside it can see.
// Unwrap is what lets http.NewResponseController reach the server's own writer,
// which the event streams flush through.
type logWriter struct {
    http.ResponseWriter
    hit bool
}

func (l *logWriter) Unwrap() http.ResponseWriter { return l.ResponseWriter }

// Log wraps a Mini App's handler so every request it serves is logged once, at
// INFO, when the handler returns:
//
//     mini app: GET /addresses?kind=rich&to=watches [123.4.5.6] [ru] [0.09 ms] [cached]
//
// name is the app the line is from and lang the language the request is answered
// in. Every request is logged, a rejected or malformed one too, and an event
// stream when it closes, so its time is how long it was open.
func Log(name string, h http.Handler, lang func(*http.Request) string) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var started = time.Now()
        var lw = &logWriter{ResponseWriter: w}
        h.ServeHTTP(lw, r)
        var mark = ""
        if lw.hit { mark = " [cached]" }
        logging.Info("%s: %s %s [%s] [%s] [%.2f ms]%s", name, r.Method, r.RequestURI, ClientIP(r), lang(r), float64(time.Since(started).Microseconds())/1000, mark)
    })
}

// Hit marks the answer to a request Log wrapped as served from a cache.
func Hit(w http.ResponseWriter) {
    if lw, ok := w.(*logWriter); ok { lw.hit = true }
}

// ClientIP is the address a request came from. Both Mini Apps sit behind a
// Cloudflare tunnel, so the connection itself is always from localhost:
// Cloudflare names the reader in CF-Connecting-IP, and True-Client-IP is the
// same on plans that send it. Only a request that reached the server without
// either — one from the machine itself — is named by its connection.
func ClientIP(r *http.Request) string {
    if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" { return ip }
    if ip := strings.TrimSpace(r.Header.Get("True-Client-IP")); ip != "" { return ip }
    var host, _, err = net.SplitHostPort(r.RemoteAddr)
    if err != nil { return r.RemoteAddr }
    return host
}
