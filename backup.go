package main

import "context"
import "os"
import "os/exec"
import "strings"
import "syscall"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

// backupScriptTimeout bounds the post-backup script. Without it a script that
// hangs — a stalled upload, a prompt nobody answers — would wedge the backup
// goroutine and silently stop every later backup. A package var so tests shrink it.
var backupScriptTimeout = 30 * time.Minute

// startBackup copies the database to path every interval, running script (when
// set) after each copy. The first copy happens at startup rather than a full
// interval later whenever the destination is missing or already older than one
// interval: this bot is redeployed and restarted far more often than a daily
// backup fires (scripts/update.sh checks for new commits every 5 minutes), so
// waiting for the first tick after every restart could starve backups completely.
// The destination's own mtime records when the last backup ran, so that decision
// needs no extra state.
func startBackup(path string, interval time.Duration, script string) {
    go func() {
        if info, err := os.Stat(path); err != nil || time.Since(info.ModTime()) >= interval {
            backup(path, script)
        }
        var t = time.NewTicker(interval)
        defer t.Stop()
        for range t.C {
            backup(path, script)
        }
    }()
}

// backup writes a consistent snapshot of the whole database to path. bbolt's
// tx.CopyFile runs inside a read transaction, which doesn't block writers, so the
// bot keeps serving while it copies. The copy lands on a temporary file that is
// then renamed into place: CopyFile writes its destination directly, so failing
// partway (a full disk) would otherwise leave a truncated file exactly where the
// last good backup was.
func backup(path, script string) {
    if db == nil { return }
    var began = time.Now()
    var tmp = path + ".tmp"
    var err = db.View(func(tx *bbolt.Tx) error { return tx.CopyFile(tmp, 0600) })
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
