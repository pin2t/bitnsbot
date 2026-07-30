// Command dbui starts the database admin web UI as a standalone process,
// serving the same interface the main bot exposes under -dbui-listen. Use it
// when you need to inspect or edit the bbolt database without running the bot.
package main

import "context"
import "flag"
import "fmt"
import "os"
import "os/signal"
import "syscall"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/dbui"
import "bitnsbot/logging"

var dbPath = flag.String("db", "", "path to the bbolt watches database (required)")
var listenAddr = flag.String("listen", "", "listen address, e.g. 127.0.0.1:8090 (required; bind to localhost only — the UI can write any bucket)")

func main() {
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
        flag.PrintDefaults()
    }
    flag.Parse()
    if *dbPath == "" {
        logging.Fatal("-db is required")
    }
    if *listenAddr == "" {
        logging.Fatal("-listen is required")
    }
    db, err := bbolt.Open(*dbPath, 0600, nil)
    if err != nil {
        logging.Fatal("open %s: %v", *dbPath, err)
    }
    var srv = dbui.Start(db, *listenAddr)
    var ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    <-ctx.Done()
    stop()
    logging.Status("shutting down")
    var sdCtx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if err := srv.Shutdown(sdCtx); err != nil {
        logging.Err("database UI shutdown: %v", err)
    }
    if err := db.Close(); err != nil {
        logging.Err("close database: %v", err)
    }
}
