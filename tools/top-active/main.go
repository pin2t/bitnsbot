// Command top-active scans the blockchain from genesis to tip, collects every
// address, looks up its transaction count in the addrindex, and prints the top N
// addresses sorted by transaction count descending.
//
// Usage:
//
//	top-active -db=bitnsbot.db -core-url=http://127.0.0.1:8332 -top=100
package main

import "bytes"
import "context"
import "encoding/hex"
import "encoding/json"
import "flag"
import "fmt"
import "net/http"
import "os"
import "sort"
import "strings"
import "sync"
import "sync/atomic"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"
import "bitnsbot/logging"

var dbPath = flag.String("db", "bitnsbot.db", "path to the bbolt database")
var coreURL = flag.String("core-url", "", "Bitcoin Core JSON-RPC URL")
var coreUser = flag.String("core-user", "", "Bitcoin Core RPC username")
var corePass = flag.String("core-pass", "", "Bitcoin Core RPC password")
var coreCookie = flag.String("core-cookie", "", "path to Bitcoin Core .cookie file")
var topN = flag.Int("top", 100, "number of top addresses to print")
var webStatus = flag.String("web-status", "", "address for live web status page (e.g. 127.0.0.1:8084)")

type rpcClient struct {
	url    string
	client *http.Client
	auth   string
}

func newRPCClient(url, user, pass, cookieFile string) (*rpcClient, error) {
	var c = &rpcClient{url: url, client: &http.Client{
		Timeout: time.Second * 5,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 50,
			IdleConnTimeout:     180 * time.Second,
		},
	}}
	if cookieFile != "" {
		var data, err = os.ReadFile(cookieFile)
		if err != nil { return nil, fmt.Errorf("read cookie: %w", err) }
		var parts = strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
		if len(parts) != 2 { return nil, fmt.Errorf("malformed cookie file") }
		user, pass = parts[0], parts[1]
	}
	var req = &http.Request{Header: http.Header{}}
	req.SetBasicAuth(user, pass)
	c.auth = req.Header.Get("Authorization")
	return c, nil
}

func (c *rpcClient) call(ctx context.Context, method string, params []interface{}, result interface{}) error {
	if params == nil { params = []interface{}{} }
	var body, err = json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0", "id": "top-active", "method": method, "params": params,
	})
	if err != nil { return err }
	var req, reqErr = http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if reqErr != nil { return reqErr }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.auth)
	var resp, doErr = c.client.Do(req)
	if doErr != nil { return doErr }
	defer resp.Body.Close()
	var decoded struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if decoded.Error != nil {
		return fmt.Errorf("%s: %s", method, decoded.Error.Message)
	}
	if result == nil { return nil }
	return json.Unmarshal(decoded.Result, result)
}

func (c *rpcClient) getBlockCount(ctx context.Context) (int64, error) {
	var count int64
	var err = c.call(ctx, "getblockcount", nil, &count)
	return count, err
}

func (c *rpcClient) getBlockHash(ctx context.Context, height int64) (string, error) {
	var hash string
	var err = c.call(ctx, "getblockhash", []interface{}{height}, &hash)
	return hash, err
}

func (c *rpcClient) getBlockVerbose(ctx context.Context, hash string) (*blockData, error) {
	var blk blockData
	var err = c.call(ctx, "getblock", []interface{}{hash, 2}, &blk)
	if err != nil { return nil, err }
	return &blk, nil
}

type blockData struct {
	Height int64  `json:"height"`
	Time   int64  `json:"time"`
	Tx     []txData `json:"tx"`
}

type txData struct {
	Vin  []vinData  `json:"vin"`
	Vout []voutData `json:"vout"`
}

type vinData struct {
	Coinbase string       `json:"coinbase"`
	PrevOut  *prevOutData `json:"prevout"`
}

type prevOutData struct {
	ScriptPubKey spkData `json:"scriptPubKey"`
}

type voutData struct {
	ScriptPubKey spkData `json:"scriptPubKey"`
}

type spkData struct {
	Address string `json:"address"`
	Hex     string `json:"hex"`
}

type addrCount struct {
	addr  string
	count int
}

func main() {
	flag.Parse()
	var d, err = bbolt.Open(*dbPath, 0600, nil)
	if err != nil {
		logging.Fatal("open database: %v", err)
	}
	defer d.Close()
	if err := addrindex.Init(d); err != nil {
		logging.Fatal("init addrindex: %v", err)
	}
	if *coreURL == "" {
		logging.Fatal("Bitcoin Core RPC (-core-url) is required")
	}
	var rpc, rpcErr = newRPCClient(*coreURL, *coreUser, *corePass, *coreCookie)
	if rpcErr != nil {
		logging.Fatal("RPC client: %v", rpcErr)
	}
	var ctx = context.Background()
	var tctx, tcancel = context.WithTimeout(ctx, 15*time.Second)
	var tip, tipErr = rpc.getBlockCount(tctx)
	tcancel()
	if tipErr != nil {
		logging.Fatal("get tip: %v", tipErr)
	}

	var counts = make(map[string]int) // address → tx count (cached)
	var countsMu sync.Mutex
	var began = time.Now()
	const numWorkers = 16
	var processed atomic.Int64

	// web status page
	if *webStatus != "" {
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			var h = processed.Load()
			var pct float64
			if tip > 0 {
				pct = float64(h) / float64(tip) * 100
			}
			var elapsed = time.Since(began).Round(time.Second)

			countsMu.Lock()
			var list []addrCount
			for addr, cnt := range counts {
				list = append(list, addrCount{addr: addr, count: cnt})
			}
			countsMu.Unlock()

			sort.Slice(list, func(i, j int) bool {
				return list[i].count > list[j].count
			})

			fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="3">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>top-active</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0d1117;color:#c9d1d9;display:flex;justify-content:center;padding:40px 20px}
.container{max-width:900px;width:100%%}
.status{text-align:center;font-size:2rem;font-weight:600;color:#58a6ff;margin-bottom:8px}
.elapsed{text-align:center;font-size:0.875rem;color:#8b949e;margin-bottom:32px}
table{width:100%%;border-collapse:collapse}
th{text-align:left;padding:10px 16px;font-size:0.75rem;font-weight:600;text-transform:uppercase;letter-spacing:0.05em;color:#8b949e;border-bottom:1px solid #21262d}
td{padding:10px 16px;border-bottom:1px solid #21262d}
.rank{width:48px;text-align:right;color:#8b949e}
.address{font-family:"SF Mono","Fira Code",monospace;font-size:0.875rem}
.count{text-align:right;font-variant-numeric:tabular-nums}
tr:hover td{background:#161b22}
</style>
</head>
<body>
<div class="container">
<div class="status">%d / %d (%.0f%%)</div>
<div class="elapsed">elapsed %s</div>
<table>
<tr><th class="rank">#</th><th>Address</th><th class="count">Transactions</th></tr>`, h, tip, pct, elapsed)
			for i, a := range list {
				fmt.Fprintf(w, `<tr><td class="rank">%d</td><td class="address">%s</td><td class="count">%d</td></tr>`+"\n", i+1, a.addr, a.count)
			}
			fmt.Fprint(w, `</table>
</div>
</body>
</html>`)
		})
		go func() {
			fmt.Fprintf(os.Stderr, "web status listening on http://%s\n", *webStatus)
			if err := http.ListenAndServe(*webStatus, nil); err != nil {
				fmt.Fprintf(os.Stderr, "web status: %v\n", err)
			}
		}()
	}
	// progress reporter: prints every 5 seconds until progressDone is closed
	var progressDone = make(chan struct{})
	go func() {
		var ticker = time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var h = processed.Load()
				var pct float64
				if tip > 0 {
					pct = float64(h) / float64(tip) * 100
				}
				fmt.Printf("\rprocessed %d / %d (%.0f%%)", h, tip, pct)
			case <-progressDone:
				return
			}
		}
	}()
	var heights = make(chan int64, numWorkers*2)
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range heights {
				var bctx, bcancel = context.WithTimeout(ctx, 60*time.Second)
				var hash, hashErr = rpc.getBlockHash(bctx, h)
				bcancel()
				if hashErr != nil {
					fmt.Fprintf(os.Stderr, "\nblock %d hash: %v\n", h, hashErr)
					processed.Add(1)
					continue
				}
				bctx, bcancel = context.WithTimeout(ctx, 60*time.Second)
				var blk, blkErr = rpc.getBlockVerbose(bctx, hash)
				bcancel()
				if blkErr != nil {
					fmt.Fprintf(os.Stderr, "\nblock %d: %v\n", h, blkErr)
					processed.Add(1)
					continue
				}
				var seen = make(map[string]string) // address → scriptHex
				for _, tx := range blk.Tx {
					for _, vin := range tx.Vin {
						if vin.Coinbase != "" { continue }
						if vin.PrevOut != nil && vin.PrevOut.ScriptPubKey.Address != "" {
							seen[vin.PrevOut.ScriptPubKey.Address] = vin.PrevOut.ScriptPubKey.Hex
						}
					}
					for _, vout := range tx.Vout {
						if vout.ScriptPubKey.Address != "" {
							seen[vout.ScriptPubKey.Address] = vout.ScriptPubKey.Hex
						}
					}
				}
				for addr, scriptHex := range seen {
					countsMu.Lock()
					_, ok := counts[addr]
					if ok {
						countsMu.Unlock()
						continue
					}
					countsMu.Unlock() // release lock during I/O-bound Lookup
					var script, _ = hex.DecodeString(scriptHex)
					var touches, _ = addrindex.Lookup(script, 1000000000)
					var cnt = len(touches)
					countsMu.Lock()
					if len(counts) < *topN {
						counts[addr] = cnt
					} else {
						var minAddr string
						var minCnt int
						for a, c := range counts {
							if minAddr == "" || c < minCnt {
								minAddr, minCnt = a, c
							}
						}
						if cnt > minCnt {
							delete(counts, minAddr)
							counts[addr] = cnt
						}
					}
					countsMu.Unlock()
				}
				processed.Add(1)
			}
		}()
	}
	for h := int64(0); h <= tip; h++ { heights <- h }
	close(heights)
	wg.Wait()
	close(progressDone)
	fmt.Printf("\rprocessed %d / %d (100%%) in %s\n", tip, tip, time.Since(began).Round(time.Second))
	var list []addrCount
	for addr, cnt := range counts {
		list = append(list, addrCount{addr: addr, count: cnt})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].count > list[j].count
	})
	if len(list) > *topN {
		list = list[:*topN]
	}
	fmt.Printf("\nTop %d addresses by transaction count:\n\n", len(list))
	for i, a := range list {
		fmt.Printf("%d. %s — %d transactions\n", i+1, a.addr, a.count)
	}
}
