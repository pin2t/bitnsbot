package main

import "encoding/hex"
import "encoding/json"
import "fmt"
import "io"
import "net/http"
import "strings"
import "sync"
import "time"

var poolsHTTP = &http.Client{Timeout: 15 * time.Second}

// poolsURL is mempool's mining-pool definitions (name, coinbase output
// addresses, and coinbase-script tag substrings). A package var so tests can
// point it at a local server.
var poolsURL = "https://raw.githubusercontent.com/mempool/mining-pools/master/pools-v2.json"

var poolsMu sync.RWMutex
var poolByAddress = map[string]string{}
var poolTags []poolTag

type poolTag struct {
    tag  string
    name string
}

type poolDef struct {
    Name      string   `json:"name"`
    Addresses []string `json:"addresses"`
    Tags      []string `json:"tags"`
}

// loadPools fetches and parses the mining-pool definitions used to attribute a
// block to its miner. On failure the maps stay empty and miners read "Unknown".
func loadPools() error {
    logNet("pools → GET %s", poolsURL)
    var resp, err = poolsHTTP.Get(poolsURL)
    if err != nil { return err }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil { return readErr }
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("status %d", resp.StatusCode)
    }
    var defs []poolDef
    if err := json.Unmarshal(body, &defs); err != nil { return err }
    var byAddr = map[string]string{}
    var tags []poolTag
    for _, d := range defs {
        for _, a := range d.Addresses {
            byAddr[a] = d.Name
        }
        for _, t := range d.Tags {
            tags = append(tags, poolTag{tag: t, name: d.Name})
        }
    }
    poolsMu.Lock()
    poolByAddress = byAddr
    poolTags = tags
    poolsMu.Unlock()
    logInfo("loaded %d mining pools", len(defs))
    return nil
}

// minerName attributes a block to a mining pool from its coinbase: a coinbase
// output address in a pool's address set, or a pool tag substring in the
// coinbase script (hex-decoded to bytes). Returns "Unknown" when nothing matches.
func minerName(coinbaseScriptHex string, coinbaseAddrs []string) string {
    poolsMu.RLock()
    defer poolsMu.RUnlock()
    for _, a := range coinbaseAddrs {
        if name, ok := poolByAddress[a]; ok { return name }
    }
    var script, _ = hex.DecodeString(coinbaseScriptHex)
    var ascii = string(script)
    for _, t := range poolTags {
        if strings.Contains(ascii, t.tag) { return t.name }
    }
    return "Unknown"
}
