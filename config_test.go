package main

import "flag"
import "os"
import "path/filepath"
import "testing"

func TestConfig(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "test.cfg")
    var err = os.WriteFile(path, []byte("# comment\n\ncore-user=configuser\ncore-url = http://localhost:8332\nlisten=:9999\n"), 0600)
    if err != nil { t.Fatal(err) }
    var oldUser, oldURL, oldListen = *coreUser, *coreURL, *listenAddr
    defer func() { *coreUser = oldUser; *coreURL = oldURL; *listenAddr = oldListen }()
    flag.Set("listen", ":7777")
    if err := applyConfig(path); err != nil {
        t.Fatalf("applyConfig: %v", err)
    }
    if *coreUser != "configuser" {
        t.Fatalf("expected core-user from config, got %q", *coreUser)
    }
    if *coreURL != "http://localhost:8332" {
        t.Fatalf("expected core-url from config with spaces trimmed, got %q", *coreURL)
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
