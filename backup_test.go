package main

import "os"
import "path/filepath"
import "strings"
import "testing"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/watches"

// openBackupDB opens a database holding one watch, so a backup has something
// recognisable in it.
func openBackupDB(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    t.Cleanup(func() { closeDB() })
    if err := watches.Add(7, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "Savings"); err != nil {
        t.Fatalf("seed watch: %v", err)
    }
}

// readBackup opens a backup file and returns the raw records in its watches
// bucket, proving the copy is a valid, complete bbolt database that can be opened
// and read on its own — not just a file of the right size.
func readBackup(t *testing.T, path string) []string {
    var copied, err = bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
    if err != nil {
        t.Fatalf("open backup: %v", err)
    }
    defer copied.Close()
    var got []string
    copied.View(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("watches")).ForEach(func(k, v []byte) error {
            got = append(got, string(v))
            return nil
        })
    })
    return got
}

func hasWatch(records []string) bool {
    return len(records) == 1 &&
        strings.Contains(records[0], "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa") &&
        strings.Contains(records[0], "Savings")
}

func TestBackup(t *testing.T) {
    openBackupDB(t)
    var path = filepath.Join(t.TempDir(), "backup.db")
    backup(path, "")
    if !hasWatch(readBackup(t, path)) {
        t.Fatalf("the watch is not in the backup: %#v", readBackup(t, path))
    }
    // the temporary file the copy goes through must not be left behind
    if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
        t.Fatalf("temporary file was left behind: %v", err)
    }
}

// A failed copy must not destroy the previous good backup — the reason the copy
// lands on a temporary file first. Here the temporary path is occupied by a
// directory, so bbolt cannot create its file there.
func TestBackupKeepsPreviousOnFailure(t *testing.T) {
    openBackupDB(t)
    var path = filepath.Join(t.TempDir(), "backup.db")
    backup(path, "")
    var before, err = os.Stat(path)
    if err != nil { t.Fatalf("first backup: %v", err) }
    if err := os.Mkdir(path+".tmp", 0700); err != nil { t.Fatalf("block temp path: %v", err) }
    backup(path, "")
    var after, serr = os.Stat(path)
    if serr != nil { t.Fatalf("previous backup was destroyed: %v", serr) }
    if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
        t.Fatalf("previous backup was overwritten by a failed copy")
    }
    if !hasWatch(readBackup(t, path)) {
        t.Fatalf("previous backup is no longer intact")
    }
}

// The script runs after the copy and is told where the backup landed, both as $1
// and in the environment.
func TestBackupScript(t *testing.T) {
    openBackupDB(t)
    var dir = t.TempDir()
    var path = filepath.Join(dir, "backup.db")
    var marker = filepath.Join(dir, "ran")
    backup(path, "printf '%s %s' \"$1\" \"$BACKUP_FILE\" > "+marker)
    var got, err = os.ReadFile(marker)
    if err != nil {
        t.Fatalf("script did not run: %v", err)
    }
    if string(got) != path+" "+path {
        t.Fatalf("script got %q, want the backup path twice (%q)", string(got), path)
    }
}

// A script that hangs must not wedge the backup goroutine forever, or every later
// backup silently stops happening. The script here backgrounds a child and waits
// on it — the case that used to defeat the timeout completely, because killing
// only the shell leaves that child holding the output pipe and CombinedOutput
// blocks until every writer to it is gone.
func TestBackupScriptTimeout(t *testing.T) {
    openBackupDB(t)
    var saved = backupScriptTimeout
    t.Cleanup(func() { backupScriptTimeout = saved })
    backupScriptTimeout = 200 * time.Millisecond
    var dir = t.TempDir()
    var path = filepath.Join(dir, "backup.db")
    var marker = filepath.Join(dir, "survived")
    var began = time.Now()
    var done = make(chan struct{})
    go func() {
        backup(path, "(sleep 2; echo yes > "+marker+") & wait")
        close(done)
    }()
    select {
    case <-done:
    case <-time.After(10 * time.Second):
        t.Fatal("backup did not return after the script timeout")
    }
    if elapsed := time.Since(began); elapsed > time.Second {
        t.Fatalf("backup took %s; the timeout should have freed it in ~200ms", elapsed)
    }
    // and the child must have been killed along with the shell, not left running
    time.Sleep(2500 * time.Millisecond)
    if _, err := os.Stat(marker); !os.IsNotExist(err) {
        t.Fatalf("the script's background child outlived the timeout: %v", err)
    }
}

// No script configured means nothing is run at all.
func TestBackupWithoutScript(t *testing.T) {
    openBackupDB(t)
    var path = filepath.Join(t.TempDir(), "backup.db")
    backup(path, "")
    if _, err := os.Stat(path); err != nil {
        t.Fatalf("backup missing: %v", err)
    }
}

// A backup that goes stale while the bot is running is retaken at the next
// check, not one whole interval after the process started. The file here begins
// at 80% of the interval — not yet due, so the startup check leaves it — which is
// exactly the case a per-interval ticker got wrong: it would have waited a
// further full interval and let the copy reach nearly twice the age asked for.
func TestStartBackupRetakesWhenStale(t *testing.T) {
    openBackupDB(t)
    var path = filepath.Join(t.TempDir(), "backup.db")
    if err := os.WriteFile(path, []byte("previous backup"), 0600); err != nil { t.Fatal(err) }
    var aged = time.Now().Add(-1600 * time.Millisecond)
    if err := os.Chtimes(path, aged, aged); err != nil { t.Fatal(err) }
    var old = backupCheck
    backupCheck = 50 * time.Millisecond
    defer func() { backupCheck = old }()
    // the goroutine has to be stopped, not left running: it reads the shared db
    // handle, which the next test reopens
    var _, stop = startBackup(path, 2*time.Second, "")
    defer stop()
    // due 400ms in; a ticker of one interval would not come round until 2s
    var deadline = time.Now().Add(1200 * time.Millisecond)
    for time.Now().Before(deadline) {
        if info, err := os.Stat(path); err == nil && info.ModTime().After(aged) {
            // and what replaced the stale file is a real database, not a partial copy
            if !hasWatch(readBackup(t, path)) {
                t.Fatalf("the retaken backup is not a valid database: %#v", readBackup(t, path))
            }
            return
        }
        time.Sleep(10 * time.Millisecond)
    }
    t.Fatal("a backup that went stale was not retaken before a whole interval had passed")
}

// The check rate is the smaller of the hour and the interval, so a -backup-interval
// shorter than an hour is not held back to one.
func TestBackupCheckRate(t *testing.T) {
    // a fresh file, so neither goroutine finds a backup due while it runs
    var path = filepath.Join(t.TempDir(), "backup.db")
    if err := os.WriteFile(path, []byte("fresh"), 0600); err != nil { t.Fatal(err) }
    var daily, stopDaily = startBackup(path, 24*time.Hour, "")
    defer stopDaily()
    if daily != time.Hour {
        t.Errorf("a daily backup is checked every %s, want hourly", daily)
    }
    var half, stopHalf = startBackup(path, 30*time.Minute, "")
    defer stopHalf()
    if half != 30*time.Minute {
        t.Errorf("a 30m backup is checked every %s, want every 30m", half)
    }
}

// startBackup copies immediately when the destination is missing or stale, so a
// bot that restarts more often than the interval still gets backed up.
func TestStartBackupRunsWhenDue(t *testing.T) {
    openBackupDB(t)
    var path = filepath.Join(t.TempDir(), "backup.db")
    var _, stop = startBackup(path, time.Hour, "")
    var deadline = time.Now().Add(5 * time.Second)
    for time.Now().Before(deadline) {
        if _, err := os.Stat(path); err == nil { break }
        time.Sleep(10 * time.Millisecond)
    }
    var info, err = os.Stat(path)
    if err != nil {
        t.Fatalf("no backup was taken at startup: %v", err)
    }
    // a fresh backup is not due again, so a restart within the interval leaves it
    var before = info.ModTime()
    stop()
    var _, stopAgain = startBackup(path, time.Hour, "")
    defer stopAgain()
    time.Sleep(300 * time.Millisecond)
    var after, serr = os.Stat(path)
    if serr != nil { t.Fatalf("stat: %v", serr) }
    if !after.ModTime().Equal(before) {
        t.Fatalf("a fresh backup was redone within the interval")
    }
}
