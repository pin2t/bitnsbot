// Command addresses scans the blockchain from genesis to tip, collects every
// address, looks up its transaction count in the addrindex, and stores the
// result in a new bbolt bucket "addresses" keyed by address string, with a
// json-encoded value (extensible for future fields).
//
// Usage:
//
//	addresses -db=bitnsbot.db -core-url=http://127.0.0.1:8332
package main

import "bytes"
import "context"
import "encoding/hex"
import "encoding/json"
import "flag"
import "fmt"
import "net/http"
import "os"
import "strconv"
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

var addressesBucket = []byte("addresses")
var addressesCursorBucket = []byte("addresses-cursor")

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

// AddressInfo is the json-encoded value stored per address. New fields can
// be added at the end; old decoders will ignore unknown fields.
type AddressInfo struct {
	Txs int `json:"transactions"`
}

type addrEntry struct {
	addr    string
	txCount int
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
	// ensure the addresses and cursor buckets exist
	if err := d.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{addressesBucket, addressesCursorBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		logging.Fatal("create buckets: %v", err)
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
	// read the last processed height from the addresses-cursor bucket
	var start int64
	if err := d.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket(addressesCursorBucket).Get([]byte("cursor")); v != nil {
			var e error
			start, e = strconv.ParseInt(string(v), 10, 64)
			if e != nil { return e }
			start++ // resume from the next block
		}
		return nil
	}); err != nil {
		logging.Fatal("read cursor: %v", err)
	}
	var began = time.Now()
	const numWorkers = 16
	const batchSize = 1000
	var processed atomic.Int64
	// processedBlocks tracks which block heights have been fully processed
	// by workers. The collector uses it to advance the cursor past every
	// consecutive block that completed, so an interrupted run resumes from
	// the first gap instead of restarting.
	var processedMu sync.Mutex
	var processedBlocks = make(map[int64]struct{})
	var cursor = start - 1 // last committed cursor value
	// collector receives (addr, txCount) from workers, deduplicates, and
	// flushes to bbolt in batches of batchSize.
	var entries = make(chan addrEntry, 10000)
	var collectorDone = make(chan struct{})
	go func() {
		defer close(collectorDone)
		var seen = make(map[string]bool)
		var batch []addrEntry
		var totalWritten int64
		flush := func() {
			if len(batch) == 0 { return }
			if err := d.Update(func(tx *bbolt.Tx) error {
				var b = tx.Bucket(addressesBucket)
				for _, e := range batch {
					if b.Get([]byte(e.addr)) != nil { continue }
					var info = AddressInfo{Txs: e.txCount}
					var val, err = json.Marshal(info)
					if err != nil { return err }
					if err := b.Put([]byte(e.addr), val); err != nil { return err }
				}
				// advance cursor past every consecutive processed block
				processedMu.Lock()
				for {
					if _, ok := processedBlocks[cursor+1]; ok {
						cursor++
					} else {
						break
					}
				}
				processedMu.Unlock()
				return tx.Bucket(addressesCursorBucket).Put(
					[]byte("cursor"), []byte(strconv.FormatInt(cursor, 10)))
			}); err != nil {
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
		flush() // final partial batch
		fmt.Fprintf(os.Stderr, "collector finished: %d addresses written\n", totalWritten)
	}()
	// progress reporter
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
	// worker pool
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
	close(entries) // signal collector to flush remaining and exit
	<-collectorDone
	fmt.Printf("\rprocessed %d / %d (100%%) in %s\n", tip, tip, time.Since(began).Round(time.Second))
}
