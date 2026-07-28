package main

import "context"
import "encoding/json"
import "fmt"
import "io"
import "net/http"
import "time"

import "bitnsbot/addrindex"
import "bitnsbot/logging"

// restSource builds addrindex.Block values entirely from Bitcoin Core's REST
// interface (-rest=1) — no dependency on the JSON-RPC client, so the address
// index can run before the rest of the Core migration is wired up. REST is
// served on the same host:port as JSON-RPC and needs no authentication (its only
// access control is whether -rest is enabled at all).
//
// This is also the piece that makes a full-chain backfill tractable at all:
// /rest/block/<hash>.bin and /rest/spenttxouts/<hash>.bin return raw bytes with
// none of the JSON serialization cost that makes getblock verbosity 3 ~30×
// slower than verbosity 0 (confirmed against a real Core v31.1.0 node; see
// CLAUDE.md's Address indexing section for the measurements). electrs' current
// backend (bindex-rs) uses the same two endpoints for the same reason.
type restSource struct {
    baseURL string
    client  *http.Client
}

func newRESTSource(baseURL string) *restSource {
    return &restSource{baseURL: baseURL, client: &http.Client{Timeout: 30 * time.Second}}
}

func (s *restSource) get(ctx context.Context, path string) ([]byte, error) {
    var req, err = http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
    if err != nil { return nil, err }
    var resp, doErr = s.client.Do(req)
    if doErr != nil { return nil, doErr }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
    }
    return io.ReadAll(resp.Body)
}

func (s *restSource) Tip(ctx context.Context) (int, error) {
    var body, err = s.get(ctx, "/rest/chaininfo.json")
    if err != nil { return 0, err }
    var info struct {
        Blocks int `json:"blocks"`
    }
    if err := json.Unmarshal(body, &info); err != nil { return 0, err }
    return info.Blocks, nil
}

func (s *restSource) BlockAt(ctx context.Context, height int) (addrindex.Block, error) {
    var hashBytes, herr = s.get(ctx, fmt.Sprintf("/rest/blockhashbyheight/%d.bin", height))
    if herr != nil { return addrindex.Block{}, herr }
    if len(hashBytes) != 32 {
        return addrindex.Block{}, fmt.Errorf("blockhashbyheight %d: got %d bytes, want 32", height, len(hashBytes))
    }
    var hash = reverseHex(hashBytes)
    var raw, rerr = s.get(ctx, "/rest/block/"+hash+".bin")
    if rerr != nil { return addrindex.Block{}, rerr }
    var spent, serr = s.get(ctx, "/rest/spenttxouts/"+hash+".bin")
    if serr != nil { return addrindex.Block{}, serr }
    return addrindex.Block{Hash: hash, Raw: raw, Spent: spent}, nil
}

// startAddrIndex launches the address-index backfill against Core's REST
// interface, logging plainly if -rest isn't enabled rather than failing
// startup — the index is a convenience for /info <address> history, not a
// dependency anything else needs.
func startAddrIndex(restBaseURL string) {
    var src = newRESTSource(restBaseURL)
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if _, err := src.Tip(ctx); err != nil {
        logging.Warn("address index disabled: Core's REST interface is unreachable at %s (%v) — enable -rest=1", restBaseURL, err)
        return
    }
    addrindex.StartBackfill(src)
}
