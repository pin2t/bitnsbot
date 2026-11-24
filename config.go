package main

import "fmt"
import "strings"
import "time"
import "github.com/BurntSushi/toml"

// config is every setting the bot takes apart from -config and -verbose,
// read from the TOML file -config names. Each section is a struct of its own,
// and a key the struct does not name is an error rather than being skipped:
// a misspelled key would otherwise leave its setting at the default with
// nothing said.
type config struct {
    Core     coreConfig     `toml:"core"`
    Telegram telegramConfig `toml:"telegram"`
    Max      maxConfig      `toml:"max"`
    DB       dbConfig       `toml:"db"`
}

// coreConfig is Bitcoin Core's JSON-RPC endpoint, its credentials (a user and
// password, or the cookie file Core writes into its data directory) and the
// ZMQ endpoints publishing hashblock, rawtx and sequence. An empty URL runs
// the bot without a node.
type coreConfig struct {
    URL      string   `toml:"url"`
    User     string   `toml:"user"`
    Password string   `toml:"password"`
    Cookie   string   `toml:"cookie"`
    ZMQ      []string `toml:"zmq"`
}

// appConfig is a Mini App's web server; an empty listen address disables it.
type appConfig struct {
    Listen string `toml:"listen"`
}

// telegramConfig is the Telegram bot: its token, the webhook server, the URL
// registered with setWebhook (whose path is the one the webhook server
// accepts updates on), the Bot API server it calls, and its Mini App.
// Telegram starts only with a token.
type telegramConfig struct {
    Token           string    `toml:"token"`
    Listen          string    `toml:"listen"`
    WebhookURL      string    `toml:"webhookURL"`
    API             string    `toml:"api"`
    Secret          string    `toml:"secret"`
    RegisterWebhook bool      `toml:"registerWebhook"`
    App             appConfig `toml:"app"`
}

// maxConfig is the MAX bot: its token, the webhook server, the public URL
// subscribed on startup, the secret checked on each update, and its Mini App.
// MAX starts only with a token.
type maxConfig struct {
    Token      string    `toml:"token"`
    Listen     string    `toml:"listen"`
    WebhookURL string    `toml:"webhookURL"`
    Secret     string    `toml:"secret"`
    App        appConfig `toml:"app"`
}

// dbConfig is the bot's SQLite database and its periodic backups.
type dbConfig struct {
    Path   string       `toml:"path"`
    Backup backupConfig `toml:"backup"`
}

// backupConfig is where the database is copied to (empty disables backups),
// how old that copy may get, and a command run after each copy with its path
// as $1 and in $BACKUP_FILE.
type backupConfig struct {
    Path     string        `toml:"path"`
    Interval time.Duration `toml:"interval"`
    Script   string        `toml:"script"`
}

// defaults is the configuration a file that sets nothing gives.
func defaults() config {
    return config{
        Telegram: telegramConfig{
            Listen:          ":8082",
            API:             "http://localhost:8081",
            RegisterWebhook: true,
            App:             appConfig{Listen: "127.0.0.1:8080"},
        },
        Max: maxConfig{
            Listen: ":8084",
            App:    appConfig{Listen: "127.0.0.1:8085"},
        },
        DB: dbConfig{
            Path:   "bitnsbot.db",
            Backup: backupConfig{Interval: 24 * time.Hour},
        },
    }
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
