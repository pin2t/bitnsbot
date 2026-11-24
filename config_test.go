package main

import "os"
import "path/filepath"
import "strings"
import "testing"
import "time"

// writeConfig puts text in a file of its own and returns its path.
func writeConfig(t *testing.T, text string) string {
    var path = filepath.Join(t.TempDir(), "bitnsbot.toml")
    if err := os.WriteFile(path, []byte(text), 0o600); err != nil { t.Fatal(err) }
    return path
}

// TestConfigSections reads every section, and checks that a key the file
// leaves out keeps its default.
func TestConfigSections(t *testing.T) {
    var path = writeConfig(t, `
[core]
url = "http://127.0.0.1:8332"
user = "user"
password = "pass"
cookie = "/home/pi/.bitcoin/.cookie"
zmq = ["tcp://127.0.0.1:28332", "tcp://127.0.0.1:28333"]

[telegram]
token = "tg-token"
webhookURL = "http://localhost:8082/bot"
secret = "s"
registerWebhook = false

[telegram.app]
listen = "127.0.0.1:9080"

[max]
token = "max-token"
webhookURL = "https://example.org/max"

[max.app]
listen = ""

[db]
path = "/opt/bitnsbot/bitnsbot.db"

[db.backup]
path = "/opt/bitnsbot/bitnsbot.db.bak"
interval = "12h"
script = "rclone copy \"$1\" remote:"
`)
    var c, err = loadConfig(path)
    if err != nil { t.Fatal(err) }
    if c.Core.URL != "http://127.0.0.1:8332" || c.Core.User != "user" || c.Core.Password != "pass" || c.Core.Cookie != "/home/pi/.bitcoin/.cookie" {
        t.Errorf("core = %+v", c.Core)
    }
    if len(c.Core.ZMQ) != 2 || c.Core.ZMQ[1] != "tcp://127.0.0.1:28333" { t.Errorf("zmq = %v", c.Core.ZMQ) }
    if c.Telegram.Token != "tg-token" || c.Telegram.WebhookURL != "http://localhost:8082/bot" || c.Telegram.Secret != "s" || c.Telegram.RegisterWebhook {
        t.Errorf("telegram = %+v", c.Telegram)
    }
    if c.Telegram.Listen != ":8082" || c.Telegram.API != "http://localhost:8081" {
        t.Errorf("telegram defaults lost: %+v", c.Telegram)
    }
    if c.Telegram.App.Listen != "127.0.0.1:9080" { t.Errorf("telegram.app.listen = %q", c.Telegram.App.Listen) }
    if c.Max.Token != "max-token" || c.Max.WebhookURL != "https://example.org/max" || c.Max.Listen != ":8084" {
        t.Errorf("max = %+v", c.Max)
    }
    if c.Max.App.Listen != "" { t.Errorf("max.app.listen = %q, want it disabled", c.Max.App.Listen) }
    if c.DB.Path != "/opt/bitnsbot/bitnsbot.db" { t.Errorf("db.path = %q", c.DB.Path) }
    if c.DB.Backup.Path != "/opt/bitnsbot/bitnsbot.db.bak" || c.DB.Backup.Interval != 12*time.Hour || c.DB.Backup.Script != `rclone copy "$1" remote:` {
        t.Errorf("db.backup = %+v", c.DB.Backup)
    }
}

// TestConfigDefaults checks that no file at all is the defaults.
func TestConfigDefaults(t *testing.T) {
    var c, err = loadConfig("")
    if err != nil { t.Fatal(err) }
    if c.DB.Path != "bitnsbot.db" || c.DB.Backup.Interval != 24*time.Hour || !c.Telegram.RegisterWebhook || c.Telegram.App.Listen != "127.0.0.1:8080" || c.Max.App.Listen != "127.0.0.1:8085" {
        t.Errorf("defaults = %+v", c)
    }
}

// TestConfigRejectsUnknownKeys checks that a misspelled key, and one from a
// section that does not exist, fail rather than being skipped.
func TestConfigRejectsUnknownKeys(t *testing.T) {
    var path = writeConfig(t, "[core]\nurll = \"x\"\n\n[apps]\nlisten = \"y\"\n")
    var _, err = loadConfig(path)
    if err == nil { t.Fatal("no error for unknown keys") }
    if !strings.Contains(err.Error(), "core.urll") || !strings.Contains(err.Error(), "apps.listen") {
        t.Errorf("error = %v, want both keys named", err)
    }
}

// TestConfigRejectsOldFormat checks that a name=value file from before TOML
// fails to load instead of starting with nothing set.
func TestConfigRejectsOldFormat(t *testing.T) {
    var path = writeConfig(t, "botToken=abc\ncoreURL=http://127.0.0.1:8332\n")
    if _, err := loadConfig(path); err == nil { t.Fatal("old format accepted") }
}

// TestConfigRejectsBadDuration checks that an interval that is not a
// duration is an error.
func TestConfigRejectsBadDuration(t *testing.T) {
    var path = writeConfig(t, "[db.backup]\ninterval = \"daily\"\n")
    if _, err := loadConfig(path); err == nil { t.Fatal("bad interval accepted") }
}
