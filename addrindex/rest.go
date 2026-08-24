package addrindex

import "context"
import "encoding/hex"
import "encoding/json"
import "fmt"
import "io"
import "net/http"
import "time"

// REST builds Block values entirely from Bitcoin Core's REST interface
// (-rest=1). REST is served on the same host:port as JSON-RPC and needs no
// authentication — its only access control is whether -rest is enabled at all.
//
// This is the piece that makes a full-chain backfill tractable: /rest/block and
// /rest/spenttxouts return raw bytes with none of the JSON serialization cost
// that makes getblock verbosity 3 ~30x slower (measured against a real Core
// v31.1.0 node; see CLAUDE.md's Address indexing section). electrs' current
// backend (bindex-rs) uses the same two endpoints for the same reason.
//
// It lives here rather than in the caller because the endpoints and their binary
// formats are how this index is built: the bot and tools/addrindex both drive
// the backfill, and a second copy would be free to drift from the parser.
type REST struct {
    baseURL string
    client  *http.Client
}

func NewREST(baseURL string) *REST {
    return &REST{baseURL: baseURL, client: &http.Client{Timeout: 30 * time.Second}}
}

func (s *REST) get(ctx context.Context, path string) ([]byte, error) {
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

func (s *REST) Tip(ctx context.Context) (int, error) {
    var body, err = s.get(ctx, "/rest/chaininfo.json")
    if err != nil { return 0, err }
    var info struct {
        Blocks int `json:"blocks"`
    }
    if err := json.Unmarshal(body, &info); err != nil { return 0, err }
    return info.Blocks, nil
}

func (s *REST) BlockAt(ctx context.Context, height int) (Block, error) {
    var hashBytes, herr = s.get(ctx, fmt.Sprintf("/rest/blockhashbyheight/%d.bin", height))
    if herr != nil { return Block{}, herr }
    if len(hashBytes) != 32 {
        return Block{}, fmt.Errorf("blockhashbyheight %d: got %d bytes, want 32", height, len(hashBytes))
    }
    var hash = reverseHex(hashBytes)
    var raw, rerr = s.get(ctx, "/rest/block/"+hash+".bin")
    if rerr != nil { return Block{}, rerr }
    var spent, serr = s.get(ctx, "/rest/spenttxouts/"+hash+".bin")
    if serr != nil { return Block{}, serr }
    return Block{Hash: hash, Raw: raw, Spent: spent}, nil
}

// reverseHex renders a 32-byte hash the way Bitcoin displays it: serialized
// little-endian on the wire, printed big-endian. A second copy of zmq.go's, for
// the same reason the reader below is one — this package cannot import main.
func reverseHex(b []byte) string {
    var flipped = make([]byte, len(b))
    for i := range b { flipped[i] = b[len(b)-1-i] }
    return hex.EncodeToString(flipped)
}
