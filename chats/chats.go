// Package chats keeps a row for every chat the bot has heard from, on either
// platform: the language its client reports, when it first and last wrote, how
// many updates it has sent, the state it is in — a bare /info, /watch or
// /unwatch waiting for its argument — and the whole first update it sent, exactly as the platform
// delivered it.
//
// The table is keyed (platform, chat), since a Telegram chat and a MAX chat may
// share a number.
package chats

import "database/sql"
import "sync"
import "time"
import "bitnsbot/logging"
import "bitnsbot/lru"

var db *sql.DB

// Info is the state of a chat that sent a bare /info and owes it the address,
// transaction or block to look up.
const Info = "info"

// Watch is the state of a chat that sent a bare /watch and owes it the address
// or transaction to watch, with an optional alias.
const Watch = "watch"

// Unwatch is the state of a chat that sent a bare /unwatch and owes it the
// watch to stop.
const Unwatch = "unwatch"

// stateTTL is how long a state waits for the message it asks for. A chat that
// sent a bare /info and wrote again an hour later is not answering it, so past
// this the state is dropped rather than taken; a var so tests can shrink it.
var stateTTL = 10 * time.Minute

// Chat is one chat's row.
type Chat struct {
    Platform string
    ID       int64
    Lang     string
    First    time.Time
    Last     time.Time
    Messages int64
    State    string
    StateAt  time.Time
    CreateMsg string
}

type key struct {
    platform string
    chat     int64
}

// langs fronts the lang column, because every translated string in a reply asks
// for its chat's language — a dozen lookups for one /info — and Update is the only
// writer. Bounded, since anyone can message the bot.
var langs = lru.New[key, string](10000)

// langMu guards langs, and is held across a miss's query as well: Update writes
// the row before it takes the lock to cache the new language, so a lookup that
// read the old value cannot cache it after Update has cached the new one.
var langMu sync.Mutex

// Init stores the shared handle and creates the table when it is missing. The
// language cache is emptied, since a new handle is a new database.
//
// `createMsg` is the update that created the row — the chat's first — as JSON
// text; `state` is "" for a chat owing nothing, and `stateAt` the unix time it
// was last set or cleared. A table from before the column was renamed still
// calls it `body`, and is renamed here.
func Init(handle *sql.DB) error {
    db = handle
    langMu.Lock()
    langs.Clear()
    langMu.Unlock()
    if db == nil { return nil }
    var _, err = db.Exec(`create table if not exists chats (platform TEXT NOT NULL, chat INTEGER NOT NULL,
        lang TEXT NOT NULL, first INTEGER NOT NULL, last INTEGER NOT NULL, messages INTEGER NOT NULL,
        state TEXT NOT NULL, stateAt INTEGER NOT NULL, createMsg TEXT NOT NULL, PRIMARY KEY (platform, chat))`)
    if err != nil { return err }
    var old int
    if err := db.QueryRow("select count(*) from pragma_table_info('chats') where name = 'body'").Scan(&old); err != nil { return err }
    if old == 0 { return nil }
    var _, renameErr = db.Exec("alter table chats rename column body to createMsg")
    return renameErr
}

// Update records an update from a chat: a message, a tapped button or the bot
// being started. A new chat's row keeps body — the update as received — as its
// createMsg, and is never given another: a known chat's last time and count
// move, and its createMsg is left alone. lang is what
// the sender's client reports, and an update without one leaves the stored
// language alone.
func Update(platform string, chat int64, lang string, body []byte) error {
    logging.Db("update platform=%s chat=%d lang=%s", platform, chat, lang)
    var err error
    if db != nil {
        var now = time.Now().Unix()
        _, err = db.Exec(`insert into chats (platform, chat, lang, first, last, messages, state, stateAt, createMsg)
            values (?, ?, ?, ?, ?, 1, '', ?, ?)
            on conflict(platform, chat) do update set last = excluded.last, messages = chats.messages + 1,
            lang = case when excluded.lang = '' then chats.lang else excluded.lang end`,
            platform, chat, lang, now, now, now, string(body))
    }
    if lang != "" {
        langMu.Lock()
        langs.Put(key{platform, chat}, lang)
        langMu.Unlock()
    }
    return err
}

// Language is the language a chat's client last reported, "" when it never has.
func Language(platform string, chat int64) string {
    var k = key{platform, chat}
    langMu.Lock()
    defer langMu.Unlock()
    if lang, ok := langs.Get(k); ok { return lang }
    if db == nil { return "" }
    var lang string
    var err = db.QueryRow("select lang from chats where platform = ? and chat = ?", platform, chat).Scan(&lang)
    if err == sql.ErrNoRows { return "" }
    if err != nil {
        logging.Err("chat language: %v", err)
        return ""
    }
    langs.Put(k, lang)
    return lang
}

// SetState puts a chat in state, "" clearing it, and stamps stateAt with now.
// Only a chat Update has recorded has a row to hold one, and every update reaches
// Update before it is handled.
func SetState(platform string, chat int64, state string) error {
    logging.Db("set state platform=%s chat=%d state=%s", platform, chat, state)
    if db == nil { return nil }
    var _, err = db.Exec("update chats set state = ?, stateAt = ? where platform = ? and chat = ?", state, time.Now().Unix(), platform, chat)
    return err
}

// Take clears a chat's state if it is state, and reports whether it was and was
// set within stateTTL. The first statement takes a fresh state, and is what
// stops two updates handled at once both taking it; the second drops one that
// has expired, so it is reverted to none instead of lingering — reported as not
// taken, since the message in hand is not the answer it waited for.
func Take(platform string, chat int64, state string) (bool, error) {
    logging.Db("take state platform=%s chat=%d state=%s", platform, chat, state)
    if db == nil { return false, nil }
    var now = time.Now()
    var cutoff = now.Add(-stateTTL).Unix()
    var res, err = db.Exec("update chats set state = '', stateAt = ? where platform = ? and chat = ? and state = ? and stateAt >= ?",
        now.Unix(), platform, chat, state, cutoff)
    if err != nil { return false, err }
    var n, rowsErr = res.RowsAffected()
    if rowsErr != nil || n == 1 { return n == 1, rowsErr }
    var _, expireErr = db.Exec("update chats set state = '', stateAt = ? where platform = ? and chat = ? and state = ? and stateAt < ?",
        now.Unix(), platform, chat, state, cutoff)
    return false, expireErr
}

// Get is a chat's row, and whether there is one.
func Get(platform string, chat int64) (Chat, bool, error) {
    var c = Chat{Platform: platform, ID: chat}
    if db == nil { return c, false, nil }
    var first, last, stateAt int64
    var err = db.QueryRow("select lang, first, last, messages, state, stateAt, createMsg from chats where platform = ? and chat = ?",
        platform, chat).Scan(&c.Lang, &first, &last, &c.Messages, &c.State, &stateAt, &c.CreateMsg)
    if err == sql.ErrNoRows { return c, false, nil }
    if err != nil { return c, false, err }
    c.First, c.Last, c.StateAt = time.Unix(first, 0), time.Unix(last, 0), time.Unix(stateAt, 0)
    return c, true, nil
}
