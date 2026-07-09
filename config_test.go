package main

import "flag"
import "os"
import "path/filepath"
import "testing"

func TestConfig(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "test.cfg")
    var err = os.WriteFile(path, []byte("# comment\n\nbtcd-user=configuser\nbtcd-url = wss://localhost:1234/ws\nlisten=:9999\n"), 0600)
    if err != nil { t.Fatal(err) }
    var oldUser, oldURL, oldListen = *btcdUser, *btcdURL, *listenAddr
    defer func() { *btcdUser = oldUser; *btcdURL = oldURL; *listenAddr = oldListen }()
    flag.Set("listen", ":7777")
    if err := applyConfig(path); err != nil {
        t.Fatalf("applyConfig: %v", err)
    }
    if *btcdUser != "configuser" {
        t.Fatalf("expected btcd-user from config, got %q", *btcdUser)
    }
    if *btcdURL != "wss://localhost:1234/ws" {
        t.Fatalf("expected btcd-url from config with spaces trimmed, got %q", *btcdURL)
    }
    if *listenAddr != ":7777" {
        t.Fatalf("expected command-line listen to win over config, got %q", *listenAddr)
    }
    if err := applyConfig(filepath.Join(t.TempDir(), "missing.cfg")); err == nil {
        t.Fatalf("expected error for missing file")
    }
    os.WriteFile(path, []byte("unknown-option=1\n"), 0600)
    if err := applyConfig(path); err == nil {
        t.Fatalf("expected error for unknown option")
    }
    os.WriteFile(path, []byte("no equals sign\n"), 0600)
    if err := applyConfig(path); err == nil {
        t.Fatalf("expected error for malformed line")
    }
}
