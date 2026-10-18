package chats

import "database/sql"
import "path/filepath"
import "sync"
import "testing"
import "time"
import _ "modernc.org/sqlite"

func open(t *testing.T) *sql.DB {
    t.Helper()
    var handle, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "chats.db")+"?_pragma=busy_timeout(10000)")
    if err != nil { t.Fatalf("open: %v", err) }
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { handle.Close(); Init(nil) })
    return handle
}

// A chat's first update makes its row and is the only body it ever keeps; later
// ones move the last time and the count, and change the language only when they
// report one. The same number on the other platform is another chat.
func TestUpdateRecordsChat(t *testing.T) {
    open(t)
    var before = time.Now().Unix()
    if err := Update("tg", 42, "ru", []byte(`{"update_id":1}`)); err != nil { t.Fatal(err) }
    if err := Update("tg", 42, "", []byte(`{"update_id":2}`)); err != nil { t.Fatal(err) }
    var c, ok, err = Get("tg", 42)
    if err != nil || !ok { t.Fatalf("get: %v %v", ok, err) }
    if c.Lang != "ru" || c.Messages != 2 || c.Body != `{"update_id":1}` || c.State != "" {
        t.Fatalf("after two updates: %+v", c)
    }
    if c.First.Unix() < before || c.Last.Before(c.First) || c.Last.Unix() > time.Now().Unix() {
        t.Fatalf("times: first %v, last %v", c.First, c.Last)
    }
    if err := Update("tg", 42, "es", nil); err != nil { t.Fatal(err) }
    if c, _, _ = Get("tg", 42); c.Lang != "es" || c.Messages != 3 || c.Body != `{"update_id":1}` {
        t.Fatalf("after a new language: %+v", c)
    }
    if err := Update("max", 42, "", []byte(`{"update_type":"bot_started"}`)); err != nil { t.Fatal(err) }
    var other, _, _ = Get("max", 42)
    if other.Lang != "" || other.Messages != 1 || other.Body != `{"update_type":"bot_started"}` {
        t.Fatalf("the MAX chat: %+v", other)
    }
    if Language("max", 42) != "" || Language("tg", 42) != "es" { t.Fatalf("languages %q %q", Language("max", 42), Language("tg", 42)) }
    if _, ok, _ = Get("tg", 7); ok { t.Fatal("a chat never seen has a row") }
}

// The language is in the table, not only in the cache in front of it, so it
// outlives a restart — which is what puts a notification sent after one in the
// reader's language.
func TestLanguageSurvivesRestart(t *testing.T) {
    var handle = open(t)
    if err := Update("tg", 1, "ru", nil); err != nil { t.Fatal(err) }
    if err := Init(handle); err != nil { t.Fatal(err) }
    if got := Language("tg", 1); got != "ru" { t.Fatalf("language after restart %q", got) }
    if got := Language("tg", 2); got != "" { t.Fatalf("unknown chat's language %q", got) }
}

// A state is taken once: only by the state it is, and only by one of several
// updates handled at once.
func TestTakeState(t *testing.T) {
    open(t)
    if err := Update("tg", 1, "", nil); err != nil { t.Fatal(err) }
    if err := SetState("tg", 1, Info); err != nil { t.Fatal(err) }
    if ok, err := Take("tg", 1, "watch"); ok || err != nil { t.Fatalf("took another state: %v %v", ok, err) }
    if ok, err := Take("max", 1, Info); ok || err != nil { t.Fatalf("took the other platform's: %v %v", ok, err) }
    var wg sync.WaitGroup
    var mu sync.Mutex
    var took = 0
    for i := 0; i < 8; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            var ok, err = Take("tg", 1, Info)
            if err != nil { t.Error(err) }
            mu.Lock()
            if ok { took++ }
            mu.Unlock()
        }()
    }
    wg.Wait()
    if took != 1 { t.Fatalf("taken %d times", took) }
    if c, _, _ := Get("tg", 1); c.State != "" { t.Fatalf("state left %q", c.State) }
}

// A state set longer ago than stateTTL is not taken: it is reverted to none, so
// the message in hand is not read as the answer and nothing is left pending.
// SetState stamps stateAt, and taking or expiring a state stamps it again.
func TestTakeExpires(t *testing.T) {
    var handle = open(t)
    if err := Update("tg", 1, "", nil); err != nil { t.Fatal(err) }
    var before = time.Now().Unix()
    if err := SetState("tg", 1, Info); err != nil { t.Fatal(err) }
    var c, _, _ = Get("tg", 1)
    if c.State != Info || c.StateAt.Unix() < before { t.Fatalf("after SetState: %+v", c) }
    var old = time.Now().Add(-stateTTL - time.Minute).Unix()
    if _, err := handle.Exec("update chats set stateAt = ? where platform = 'tg' and chat = 1", old); err != nil { t.Fatal(err) }
    if ok, err := Take("tg", 1, Info); ok || err != nil { t.Fatalf("took an expired state: %v %v", ok, err) }
    if c, _, _ = Get("tg", 1); c.State != "" || c.StateAt.Unix() <= old { t.Fatalf("expired state left: %+v", c) }
    if ok, _ := Take("tg", 1, Info); ok { t.Fatal("took a state that was dropped") }
    if err := SetState("tg", 1, Info); err != nil { t.Fatal(err) }
    var edge = time.Now().Add(-stateTTL + time.Minute).Unix()
    if _, err := handle.Exec("update chats set stateAt = ? where platform = 'tg' and chat = 1", edge); err != nil { t.Fatal(err) }
    if ok, err := Take("tg", 1, Info); !ok || err != nil { t.Fatalf("did not take a state inside the TTL: %v %v", ok, err) }
}
