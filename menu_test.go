package main

import "encoding/json"
import "net/http"
import "net/http/httptest"
import "strings"
import "testing"

// The menu and /start must list exactly the same commands with the same
// descriptions — that is the whole point of driving both off `commands`.
func TestMenuMatchesStart(t *testing.T) {
    var sent []string
    var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Text string `json:"text"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, body.Text)
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer server.Close()
    update(newBot("TESTTOKEN", server.URL), Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/start"}})
    if len(sent) != 1 {
        t.Fatalf("expected one /start reply, got %#v", sent)
    }
    for _, c := range commands {
        if !strings.Contains(sent[0], "<b>/"+c.name+"</b>") {
            t.Errorf("/start does not list command %q", c.name)
        }
        if d := menuDescription(c.line); !strings.Contains(sent[0], d) {
            t.Errorf("/start does not carry the menu description for %q: %q", c.name, d)
        }
    }
}

// Every command the dispatcher answers must be in the menu, and vice versa —
// otherwise the menu offers a command that does nothing, or hides a real one.
func TestMenuCoversEveryDispatchedCommand(t *testing.T) {
    var dispatched = []string{"start", "info", "watch", "unwatch", "watches", "fees", "mempool", "miners", "market"}
    if len(commands) != len(dispatched) {
        t.Fatalf("menu has %d commands, dispatcher answers %d", len(commands), len(dispatched))
    }
    var inMenu = map[string]bool{}
    for _, c := range commands { inMenu[c.name] = true }
    for _, d := range dispatched {
        if !inMenu[d] {
            t.Errorf("dispatcher answers /%s but the menu does not list it", d)
        }
    }
}

// i18n-vet only sees string literals passed straight to i18n(...).String(), so
// moving the /start lines into `commands` took them out of its reach. This
// covers the same ground: every line translated in every language.
func TestCommandLinesTranslated(t *testing.T) {
    for lang, tr := range langTrans {
        for _, c := range commands {
            if _, ok := tr[c.line]; !ok {
                t.Errorf("%s: no translation for %q", lang, c.line)
            }
        }
    }
}

// Telegram rejects the whole setMyCommands call if any entry breaks its limits,
// which would silently leave the menu empty.
func TestMenuCommandsWithinTelegramLimits(t *testing.T) {
    for _, c := range commands {
        if c.name == "" || len(c.name) > 32 || strings.ToLower(c.name) != c.name || strings.HasPrefix(c.name, "/") {
            t.Errorf("command %q is not a bare lowercase name of at most 32 characters", c.name)
        }
        for lang, tr := range langTrans {
            var d = menuDescription(tr.String(c.line))
            if d == "" || len(d) > 256 {
                t.Errorf("%s: description for %q is %d bytes, want 1..256", lang, c.name, len(d))
            }
        }
    }
}

// registerMenu publishes one list per language: the default (no language_code)
// plus one for each translation, each carrying that language's descriptions.
func TestRegisterMenuPerLanguage(t *testing.T) {
    type menuCall struct {
        Commands []struct {
            Command     string `json:"command"`
            Description string `json:"description"`
        } `json:"commands"`
        LanguageCode string `json:"language_code"`
    }
    var calls []menuCall
    var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if strings.HasSuffix(r.URL.Path, "/setMyCommands") {
            var c menuCall
            json.NewDecoder(r.Body).Decode(&c)
            calls = append(calls, c)
        }
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer server.Close()
    registerMenu(newBot("TESTTOKEN", server.URL))
    if len(calls) != 1+len(langTrans) {
        t.Fatalf("got %d setMyCommands calls, want %d (default plus one per language)", len(calls), 1+len(langTrans))
    }
    var byLang = map[string]menuCall{}
    for _, c := range calls { byLang[c.LanguageCode] = c }
    for lang, c := range byLang {
        if len(c.Commands) != len(commands) {
            t.Errorf("language %q registered %d commands, want %d", lang, len(c.Commands), len(commands))
        }
        for i, got := range c.Commands {
            if got.Command != commands[i].name {
                t.Errorf("language %q command %d = %q, want %q", lang, i, got.Command, commands[i].name)
            }
        }
    }
    if d := byLang[""].Commands[len(commands)-1].Description; d != "show this message" {
        t.Errorf("default menu /start description = %q, want the English text", d)
    }
    if _, ok := langTrans["ru"]; ok {
        var got = byLang["ru"].Commands[len(commands)-1].Description
        if got == "" || got == "show this message" {
            t.Errorf("ru menu /start description = %q, want the Russian translation", got)
        }
    }
}
