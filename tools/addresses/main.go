// Command addresses scans the blockchain from genesis to tip, collects every
// address, looks up its transaction count in the addrindex, and stores the
// result in a table named addresses, one row per address with its transaction
// count. Its place is a row of the cursors table under "addresses", so an
// interrupted run resumes where it stopped.
//
// Usage:
//
//	addresses -db=bitnsbot.sqlite -core-url=http://127.0.0.1:8332
package main

import "bytes"
import "context"
import "database/sql"
import "encoding/hex"
import "encoding/json"
import "flag"
import "fmt"
import "net/http"
import "os"
import "strings"
import "sync"
import "sync/atomic"
import "time"
import _ "modernc.org/sqlite"
import "bitnsbot/addrindex"
import "bitnsbot/cursors"
import "bitnsbot/logging"

var dbPath = flag.String("db", "bitnsbot.sqlite", "path to the SQLite database holding the address index")
var coreURL = flag.String("core-url", "", "Bitcoin Core JSON-RPC URL")
var coreUser = flag.String("core-user", "", "Bitcoin Core RPC username")
var corePass = flag.String("core-pass", "", "Bitcoin Core RPC password")
var coreCookie = flag.String("core-cookie", "", "path to Bitcoin Core .cookie file")

// addressesCursor is this scan's name in the cursors table.
const addressesCursor = "addresses"

type rpcClient struct {
	url    string
	client *http.Client
	auth   string
}

func newRPCClient(url, user, pass, cookieFile string) (*rpcClient, error) {
	var c = &rpcClient{url: url, client: &http.Client{
		Timeout: time.Second * 5,
		Transport: &http.Transport{
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     60 * time.Second,
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
		"jsonrpc": "1.0", "id": "addresses", "method": method, "params": params,
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
	Height int64   `json:"height"`
	Time   int64   `json:"time"`
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

type addrEntry struct {
	addr    string
	txCount int
}

// ensure the addresses and cursor buckets exist
//
// read the last processed height from the addresses-cursor bucket
//
// resume from the next block
//
// processedBlocks tracks which block heights have been fully processed
// by workers. The collector uses it to advance the cursor past every
// consecutive block that completed, so an interrupted run resumes from
// the first gap instead of restarting.
//
// last committed cursor value
//
// collector receives (addr, txCount) from workers, deduplicates, and
// flushes to the database in batches of batchSize.
//
// advance cursor past every consecutive processed block
//
// final partial batch
//
// progress reporter
//
// worker pool
//
// address → scriptHex
//
// signal collector to flush remaining and exit
func main() {
	flag.Parse()
	if _, err := os.Stat(*dbPath); err != nil {
		logging.Fatal("open database: %v", err)
	}
	var d, err = sql.Open("sqlite", "file:"+*dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		logging.Fatal("open database: %v", err)
	}
	defer d.Close()
	if err := cursors.Init(d); err != nil {
		logging.Fatal("init cursors: %v", err)
	}
	if err := addrindex.Init(d); err != nil {
		logging.Fatal("init addrindex: %v", err)
	}
	for _, ddl := range []string{
		`create table if not exists addresses (addr TEXT PRIMARY KEY, txs INTEGER NOT NULL)`,
		`create table if not exists cursors (name TEXT PRIMARY KEY, place INTEGER NOT NULL)`,
	} {
		if _, err := d.Exec(ddl); err != nil {
			logging.Fatal("create tables: %v", err)
		}
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
	var start int64
	if v, ok := cursors.Get(addressesCursor); ok {
		start = v + 1
	}
	var began = time.Now()
	const numWorkers = 64
	const batchSize = 5000
	var processed atomic.Int64
	var processedMu sync.Mutex
	var processedBlocks = make(map[int64]struct{})
	var cursor = start - 1
	var entries = make(chan addrEntry, 10000)
	var collectorDone = make(chan struct{})
	go func() {
		defer close(collectorDone)
		var seen = make(map[string]bool)
		var batch []addrEntry
		var totalWritten int64
		var flush = func() {
			if len(batch) == 0 { return }
			if err := func() error {
				var tx, err = d.Begin()
				if err != nil { return err }
				defer tx.Rollback()
				for _, e := range batch {
					if _, err := tx.Exec(`insert into addresses (addr, txs) values (?, ?)
						on conflict(addr) do nothing`, e.addr, e.txCount); err != nil { return err }
				}
				processedMu.Lock()
				for {
					if _, ok := processedBlocks[cursor+1]; ok {
						cursor++
					} else {
						break
					}
				}
				processedMu.Unlock()
				if err := cursors.Set(tx, addressesCursor, cursor); err != nil { return err }
				return tx.Commit()
			}(); err != nil {
				fmt.Fprintf(os.Stderr, "\nflush error: %v\n", err)
			}
			totalWritten += int64(len(batch))
			batch = batch[:0]
			seen = make(map[string]bool)
		}
		for e := range entries {
			if seen[e.addr] { continue }
			seen[e.addr] = true
			batch = append(batch, e)
			if len(batch) >= batchSize { flush() }
		}
		flush()
		fmt.Fprintf(os.Stderr, "collector finished: %d addresses written\n", totalWritten)
	}()
	var progressDone = make(chan struct{})
	go func() {
		var ticker = time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var c, _ = cursors.Get(addressesCursor)
				var pct float64
				if tip > 0 {
					pct = float64(c) / float64(tip) * 100
				}
				fmt.Printf("\rprocessed %d / %d (%.0f%%)", c, tip, pct)
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
				var seen = make(map[string]string)
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
					var script, _ = hex.DecodeString(scriptHex)
					var touches, _ = addrindex.Lookup(script, 1000000000)
					var cnt = len(touches)
					entries <- addrEntry{addr: addr, txCount: cnt}
				}
				processed.Add(1)
				processedMu.Lock()
				processedBlocks[h] = struct{}{}
				processedMu.Unlock()
			}
		}()
	}
	for h := start; h <= tip; h++ { heights <- h }
	close(heights)
	wg.Wait()
	close(progressDone)
	close(entries)
	<-collectorDone
	fmt.Printf("\rprocessed %d / %d (100%%) in %s\n", tip, tip, time.Since(began).Round(time.Second))
}
