package main

import "fmt"
import "strings"
import "time"
import "github.com/BurntSushi/toml"

// config is every setting the bot takes apart from -config and -verbose,
// read from the TOML file -config names, one nested struct per section. A key
// the struct does not name is an error rather than being skipped: a
// misspelled key would otherwise leave its setting at the default with
// nothing said.
//
// [core] is Bitcoin Core's JSON-RPC endpoint, its credentials (a user and
// password, or the cookie file Core writes into its data directory) and the
// ZMQ endpoints publishing hashblock, rawtx and sequence; an empty url runs
// the bot without a node. [telegram] and [max] are each messenger's token,
// webhook server and the URL its updates arrive at, whose path is the one the
// webhook server accepts them on; a messenger starts only with a token. Their
// .app sections are the Mini Apps' web servers, which an empty listen address
// disables. [db] is the SQLite database, and [db.backup] where it is copied to
// (empty disables backups), how old that copy may get, and a command run
// after each copy with its path as $1 and in $BACKUP_FILE.
type config struct {
    Core struct {
        URL      string   `toml:"url"`
        User     string   `toml:"user"`
        Password string   `toml:"password"`
        Cookie   string   `toml:"cookie"`
        ZMQ      []string `toml:"zmq"`
    } `toml:"core"`
    Telegram struct {
        Token           string `toml:"token"`
        Listen          string `toml:"listen"`
        WebhookURL      string `toml:"webhookURL"`
        API             string `toml:"api"`
        Secret          string `toml:"secret"`
        RegisterWebhook bool   `toml:"registerWebhook"`
        App             struct {
            Listen string `toml:"listen"`
        } `toml:"app"`
    } `toml:"telegram"`
    Max struct {
        Token      string `toml:"token"`
        Listen     string `toml:"listen"`
        WebhookURL string `toml:"webhookURL"`
        Secret     string `toml:"secret"`
        App        struct {
            Listen string `toml:"listen"`
        } `toml:"app"`
    } `toml:"max"`
    DB struct {
        Path   string `toml:"path"`
        Backup struct {
            Path     string        `toml:"path"`
            Interval time.Duration `toml:"interval"`
            Script   string        `toml:"script"`
        } `toml:"backup"`
    } `toml:"db"`
}

// defaults is the configuration a file that sets nothing gives.
func defaults() config {
    var c config
    c.Telegram.Listen = ":8082"
    c.Telegram.API = "http://localhost:8081"
    c.Telegram.RegisterWebhook = true
    c.Telegram.App.Listen = "127.0.0.1:8080"
    c.Max.Listen = ":8084"
    c.Max.App.Listen = "127.0.0.1:8085"
    c.DB.Path = "bitnsbot.db"
    c.DB.Backup.Interval = 24 * time.Hour
    return c
}

// loadConfig reads the TOML file at path over the defaults, so a key the file
// leaves out keeps its default. An empty path is the defaults alone.
func loadConfig(path string) (config, error) {
    var c = defaults()
    if path == "" { return c, nil }
    var md, err = toml.DecodeFile(path, &c)
    if err != nil { return c, fmt.Errorf("%s: %w", path, err) }
    var unknown = md.Undecoded()
    if len(unknown) == 0 { return c, nil }
    var names []string
    for _, k := range unknown {
        names = append(names, k.String())
    }
    return c, fmt.Errorf("%s: unknown settings: %s", path, strings.Join(names, ", "))
}
