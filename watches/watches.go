package watches

import "database/sql"
import "time"
import "bitnsbot/logging"

var db *sql.DB

// Watch is the public view of a stored address watch.
type Watch struct {
    Platform string
    Chat    int64
    Address string
    Alias   string
}

// The watches table is keyed by the triple — `PRIMARY KEY (platform, chat, addr)`,
// the platform being "tg" or "max", since a Telegram chat and a MAX chat may share
// a number — which is
// what stops one chat holding the same address twice, and what makes a chat's own
// watches a range of the primary key rather than a walk of the table decoding
// every row to find out whose it is. `created` is the one fact about a watch that
// is otherwise unrecoverable; nothing reads it but List's ordering. `createMsg`
// is the update the watch was made by, as JSON text — "" for one made from the
// Mini App, which has no message.
//
// Init stores the shared handle and creates the table when it is missing, and
// adds createMsg to a table from before it, its rows reading "".
func Init(handle *sql.DB) error {
    db = handle
    if db == nil { return nil }
    var _, err = db.Exec(`create table if not exists watches (platform TEXT NOT NULL, chat INTEGER NOT NULL,
        addr TEXT NOT NULL, alias TEXT NOT NULL, created INTEGER NOT NULL, createMsg TEXT NOT NULL DEFAULT '',
        PRIMARY KEY (platform, chat, addr))`)
    if err != nil { return err }
    var has int
    if err := db.QueryRow("select count(*) from pragma_table_info('watches') where name = 'createMsg'").Scan(&has); err != nil { return err }
    if has == 1 { return nil }
    var _, alterErr = db.Exec("alter table watches add column createMsg TEXT NOT NULL DEFAULT ''")
    return alterErr
}

// Add stores an address watch for a chat. Watching an address the chat already
// watches replaces the row rather than adding a second one, and **keeps the
// original `created`** — it is the same watch, and resetting its age would be a
// lie. That is the `do update` clause: an upsert that leaves those columns
// alone. msg, the update the watch is made by, is likewise kept only by the
// insert that creates the row, as its createMsg.
func Add(platform string, chatID int64, address, alias string, msg []byte) error {
    logging.Db("add platform=%s chat=%d address=%s alias=%s", platform, chatID, address, alias)
    if db == nil { return nil }
    var _, err = db.Exec(`insert into watches (platform, chat, addr, alias, created, createMsg) values (?, ?, ?, ?, ?, ?)
        on conflict(platform, chat, addr) do update set alias = excluded.alias`,
        platform, chatID, address, alias, time.Now().Unix(), string(msg))
    return err
}

// CreateMsg is the update chatID's watch on address was made by, and whether
// there is such a watch.
func CreateMsg(platform string, chatID int64, address string) (string, bool, error) {
    if db == nil { return "", false, nil }
    var msg string
    var err = db.QueryRow("select createMsg from watches where platform = ? and chat = ? and addr = ?", platform, chatID, address).Scan(&msg)
    if err == sql.ErrNoRows { return "", false, nil }
    if err != nil { return "", false, err }
    return msg, true, nil
}

// Count returns how many address watches chatID currently has on platform.
func Count(platform string, chatID int64) (int, error) {
    logging.Db("count platform=%s chat=%d", platform, chatID)
    if db == nil { return 0, nil }
    var n int
    var err = db.QueryRow("select count(*) from watches where platform = ? and chat = ?", platform, chatID).Scan(&n)
    if err != nil { return 0, err }
    return n, nil
}

// List returns every stored watch, oldest first: `created` is the order, since
// the key sorts by chat and then address. Ties break on the key, so a list of
// watches made in the same second is at least stable between calls.
func List() ([]Watch, error) {
    logging.Db("list")
    if db == nil { return nil, nil }
    var rows, err = db.Query("select platform, chat, addr, alias from watches order by created, platform, chat, addr")
    if err != nil { return nil, err }
    defer rows.Close()
    var watches []Watch
    for rows.Next() {
        var w Watch
        if err := rows.Scan(&w.Platform, &w.Chat, &w.Address, &w.Alias); err != nil { return nil, err }
        watches = append(watches, w)
    }
    return watches, rows.Err()
}

// SetAlias renames the watch on chatID's address — the chat is half the key, so
// one chat cannot rename another's — and reports whether there was one to rename.
func SetAlias(platform string, chatID int64, address, alias string) (int, error) {
    logging.Db("set alias platform=%s chat=%d address=%s alias=%s", platform, chatID, address, alias)
    if db == nil { return 0, nil }
    var res, err = db.Exec("update watches set alias = ? where platform = ? and chat = ? and addr = ?", alias, platform, chatID, address)
    if err != nil { return 0, err }
    var n, aerr = res.RowsAffected()
    return int(n), aerr
}

// Remove deletes chatID's watch on address (the chat being half the key is what
// stops one chat from removing another chat's watch) and reports whether there was
// one to delete — the caller tells "removed" from "you were not watching that" by
// the count.
func Remove(platform string, chatID int64, address string) (int, error) {
    logging.Db("remove platform=%s chat=%d address=%s", platform, chatID, address)
    if db == nil { return 0, nil }
    var res, err = db.Exec("delete from watches where platform = ? and chat = ? and addr = ?", platform, chatID, address)
    if err != nil { return 0, err }
    var n, aerr = res.RowsAffected()
    return int(n), aerr
}
