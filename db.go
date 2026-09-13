package main

import "context"
import "database/sql"
import "os"
import "os/exec"
import "strings"
import "syscall"
import "time"

import _ "modernc.org/sqlite"
import "bitnsbot/addrindex"
import "bitnsbot/addrstat"
import "bitnsbot/cursors"
import "bitnsbot/logging"
import "bitnsbot/miners"
import "bitnsbot/rates"
import "bitnsbot/watches"

// db is the bot's SQLite database, and the handle every package that stores
// anything is given. One file, one handle, one schema — the one tools/tosqlite
// writes, which is what makes that tool the upgrade path from the bbolt database
// this replaced.
var db *sql.DB

// schema is **a copy of the one in tools/tosqlite**, which is the source of truth
// for it: that tool is how a bbolt database becomes one of these, so a column it
// does not write is a column that arrives empty. The two have to be kept in step
// by hand — the same arrangement the addrindex format constants are under, and
// for the same reason: a bot cannot import a tool's package main.
//
// create table if not exists, because unlike the migration this runs on every
// start and against a database a migration may already have built.
var schema = []string{
    `create table if not exists blocks (height INTEGER PRIMARY KEY, hash TEXT NOT NULL, ts INTEGER NOT NULL,
        size INTEGER NOT NULL, txs INTEGER NOT NULL, miner TEXT NOT NULL, feesOK INTEGER NOT NULL,
        minFee INTEGER NOT NULL, avgFee INTEGER NOT NULL, maxFee INTEGER NOT NULL,
        txSizeMin INTEGER NOT NULL, txSizeAvg INTEGER NOT NULL, txSizeMax INTEGER NOT NULL,
        reward INTEGER NOT NULL, fees INTEGER NOT NULL, difficulty REAL NOT NULL)`,
    `create table if not exists market (ts INTEGER PRIMARY KEY, price INTEGER NOT NULL, cap INTEGER NOT NULL,
        volume24h INTEGER NOT NULL)`,
    `create table if not exists miners (name TEXT PRIMARY KEY, blocks INTEGER NOT NULL,
        reward INTEGER NOT NULL, fees INTEGER NOT NULL, totalWork REAL NOT NULL, lastWork REAL NOT NULL)`,
    `create table if not exists mineraddr (address TEXT PRIMARY KEY, name TEXT NOT NULL references miners(name))`,
    `create table if not exists minertag (tag TEXT PRIMARY KEY, name TEXT NOT NULL references miners(name))`,
    `create table if not exists rates (ts INTEGER PRIMARY KEY, cents INTEGER NOT NULL)`,
    `create table if not exists watches (chat INTEGER NOT NULL, addr TEXT NOT NULL, alias TEXT NOT NULL,
        created INTEGER NOT NULL, PRIMARY KEY (chat, addr))`,
    `create table if not exists addrstat (addr TEXT PRIMARY KEY, type TEXT NOT NULL, balance INTEGER NOT NULL,
        recv INTEGER NOT NULL, sent INTEGER NOT NULL, flow INTEGER NOT NULL, fees INTEGER NOT NULL,
        txs INTEGER NOT NULL, first INTEGER NOT NULL, last INTEGER NOT NULL)`,
    `create table if not exists cursors (name TEXT PRIMARY KEY, place INTEGER NOT NULL)`,
    `create table if not exists addrindex (shard INTEGER PRIMARY KEY, data BLOB NOT NULL)`,
    `create index if not exists addrstat_balance on addrstat (balance)`,
    `create index if not exists addrstat_txs on addrstat (txs)`,
    `create index if not exists addrstat_last on addrstat (last)`,
}

// dsn carries the pragmas rather than running them, because a PRAGMA applies to
// the connection that ran it and database/sql hands out a pool: in the DSN the
// driver applies them to every connection it opens.
//
// WAL is what lets the chain scans write while a webhook handler or the Mini App
// reads — bbolt's one-writer-many-readers property, which this had to keep.
// synchronous=NORMAL is WAL's safe setting (a commit survives a crash of the
// process; only a power cut can lose the last transactions, which for a cache of
// public chain data is the right trade against an fsync per commit).
// busy_timeout is what turns "database is locked" into a wait: SQLite allows one
// writer at a time, and the collectors do sometimes flush at once.
func dsn(path string) string {
    return "file:" + path +
        "?_pragma=journal_mode(WAL)" +
        "&_pragma=synchronous(NORMAL)" +
        "&_pragma=busy_timeout(10000)" +
        // on, so mineraddr and minertag cannot name a pool the miners table does
        // not have: an address is only ever attributed to a pool that exists
        "&_pragma=foreign_keys(on)"
}

// openDB opens the SQLite database, creates anything missing, and hands the
// handle to every package that stores something.
//
// Every package that owns tables must be Init'd here. Forgetting one is not a
// loud failure: those packages guard their operations on a nil handle, so the
// package silently does nothing — which is exactly how the address index came to
// fetch and parse the whole chain while storing none of it. TestOpenDBTables
// pins the full set.
func openDB(path string) error {
    logging.Db("open %s", path)
    var opened, err = sql.Open("sqlite", dsn(path))
    if err != nil { return err }
    // A few connections rather than one: reads should not queue behind a
    // thousand-block flush. Writes serialize on SQLite's own write lock, which
    // busy_timeout above is what makes them wait for rather than fail on.
    opened.SetMaxOpenConns(4)
    opened.SetMaxIdleConns(4)
    if err := opened.Ping(); err != nil { return err }
    db = opened
    for _, s := range schema {
        if _, err := db.Exec(s); err != nil { return err }
    }
    if err := cursors.Init(db); err != nil { return err }
    if err := blockInit(db); err != nil { return err }
    if err := rates.Init(db); err != nil { return err }
    if err := watches.Init(db); err != nil { return err }
    if err := miners.Init(db); err != nil { return err }
    if err := addrstat.Init(db); err != nil { return err }
    return addrindex.InitSQL(db)
}

func closeDB() error {
    if db == nil { return nil }
    var err = db.Close()
    db = nil
    return err
}

// backupScriptTimeout bounds the post-backup script. Without it a script that
// hangs — a stalled upload, a prompt nobody answers — would wedge the backup
// goroutine and silently stop every later backup. A package var so tests shrink it.
var backupScriptTimeout = 30 * time.Minute

// backupCheck is how often the goroutine asks whether a backup is due, which is
// not the same thing as how often one is taken: -backup-interval is how old a
// backup may get, and this is how often that age is looked at. A package var so
// tests shrink it.
var backupCheck = time.Hour

// startBackup keeps a copy of the database at path no older than interval,
// running script (when set) after each copy, and returns how often it checks
// plus a stop for that goroutine.
//
// The check is hourly rather than once per interval because of the deployment
// setup: scripts/update.sh restarts the service on every new commit, and a
// ticker's phase restarts with the process, so a daily one left a backup that
// was merely *not yet* due at the restart waiting a further full interval — two
// days old before being retaken. The file's own mtime is the whole state.
//
// The wait is the smaller of that and the time the copy has left, so a backup
// happens *when* it comes due. A plain ticker is not the same thing and was
// measured getting it wrong: at a 10s interval checked every 10s each check
// landed a few milliseconds before the file turned due — the wait starts before
// the copy that resets the mtime — so every other one found nothing to do and
// backups came every 20s. The cap is what still notices a backup deleted or
// replaced underneath the bot.
//
// shutdown runs the stop before closeDB, the order the webhook server is drained
// in and for the same reason: a copy in flight is reading the database.
func startBackup(path string, interval time.Duration, script string) (time.Duration, func()) {
    var every = backupCheck
    if interval < every { every = interval }
    var stop, done = make(chan struct{}), make(chan struct{})
    go func() {
        defer close(done)
        for {
            var wait = every
            if info, err := os.Stat(path); err != nil || time.Since(info.ModTime()) >= interval {
                backup(path, script)
            } else if due := interval - time.Since(info.ModTime()); due < wait {
                wait = due
            }
            select {
            case <-time.After(wait):
            case <-stop:
                return
            }
        }
    }()
    return every, func() {
        close(stop)
        <-done
    }
}

// backup writes a consistent snapshot of the whole database to path, with
// `VACUUM INTO`: SQLite builds the copy from a read transaction, so the bot keeps
// serving while it writes, and what lands is a compacted database rather than a
// byte copy — the free pages a deleted row left behind do not come along. (This
// is where bbolt's tx.CopyFile was, and it is the same shape: a consistent read,
// no writer blocked.)
//
// The copy lands on a temporary file that is then renamed into place, because
// VACUUM INTO writes its destination directly — so failing partway (a full disk)
// would otherwise leave a truncated file exactly where the last good backup was.
// It also refuses a destination that exists, which the rename is what keeps true.
func backup(path, script string) {
    if db == nil { return }
    var began = time.Now()
    var tmp = path + ".tmp"
    // VACUUM INTO refuses a destination that exists, so a temporary file left by
    // a run that died is cleared first — but only if it is a file. Anything else
    // there is something this did not put there, and removing it is not this
    // function's business; the copy then fails and the last good backup stands.
    if info, err := os.Stat(tmp); err == nil && info.Mode().IsRegular() { os.Remove(tmp) }
    var _, err = db.Exec("vacuum into ?", tmp)
    if err == nil {
        err = os.Rename(tmp, path)
    }
    if err != nil {
        os.Remove(tmp)
        logging.Err("back up database to %s: %v", path, err)
        return
    }
    var size int64
    if info, serr := os.Stat(path); serr == nil { size = info.Size() }
    logging.Status("database backed up to %s (%s bytes) in %s", path, group(size), time.Since(began).Round(time.Millisecond))
    if script == "" { return }
    var ctx, cancel = context.WithTimeout(context.Background(), backupScriptTimeout)
    defer cancel()
    // run through sh so the flag can be either a plain path to an executable
    // script or an inline command; the backup's path is passed both ways so
    // either style can find it — as $1, and in the environment as BACKUP_FILE
    var cmd = exec.CommandContext(ctx, "sh", "-c", script, "sh", path)
    cmd.Env = append(os.Environ(), "BACKUP_FILE="+path)
    // Give the script its own process group and kill the whole group on timeout.
    // Killing only the shell is not enough: anything it leaves running — a
    // backgrounded upload, a child that outlives it — inherits the output pipe,
    // and CombinedOutput blocks until every writer to that pipe is gone. So the
    // timeout would not actually free this goroutine, which is the one thing it
    // exists to do. WaitDelay then bounds the wait even if something survives the
    // signal. (Setpgid is unix-only; this bot targets Linux and macOS.)
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
    cmd.Cancel = func() error {
        var err = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
        if err == syscall.ESRCH { return os.ErrProcessDone } // exited on its own first
        return err
    }
    cmd.WaitDelay = backupScriptTimeout
    var out, runErr = cmd.CombinedOutput()
    var text = strings.TrimSpace(string(out))
    if runErr != nil {
        logging.Err("backup script: %v: %s", runErr, text)
        return
    }
    logging.Info("backup script finished: %s", text)
}
