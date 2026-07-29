// Command top-active scans the addrindex and prints the addresses most often
// touched on chain, sorted by transaction count descending.
//
// Usage:
//
//	top-active -db=bitnsbot.db -core-url=http://127.0.0.1:8332 -top=100
package main

import "bytes"
import "context"
import "encoding/binary"
import "encoding/hex"
import "encoding/json"
import "flag"
import "fmt"
import "net/http"
import "os"
import "sort"
import "strings"
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

// addrindex layout constants (must match addrindex package)
const shardLen = 2
const remainderLen = 6
const entryLen = remainderLen + 2 + 2 // remainder + heightOffset + txIndex
const rangeBlocks = 1000

type rpcClient struct {
	url    string
	client *http.Client
	auth   string
}

func newRPCClient(url, user, pass, cookieFile string) (*rpcClient, error) {
	var c = &rpcClient{url: url, client: &http.Client{}}
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

	var rpc *rpcClient
	var tip int64
	if *coreURL != "" {
		rpc, err = newRPCClient(*coreURL, *coreUser, *corePass, *coreCookie)
		if err != nil {
			logging.Fatal("RPC client: %v", err)
		}
		var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
		tip, err = rpc.getBlockCount(ctx)
		cancel()
		if err != nil {
			logging.Fatal("get tip: %v", err)
		}
	} else {
		logging.Fatal("Bitcoin Core RPC (-core-url) is required to get the tip height")
	}
	var totalRanges = tip/rangeBlocks + 1

	var counts = make(map[string]int)
	var keysProcessed int64

	var began = time.Now()
	d.View(func(tx *bbolt.Tx) error {
		var b = tx.Bucket([]byte("addrindex"))
		if b == nil {
			logging.Fatal("addrindex bucket not found — is the database empty?")
		}
		var c = b.Cursor()
		var lastReport time.Time
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if len(k) < shardLen+4 {
				continue
			}
			var shard = k[:shardLen]
			var rangeIdx = binary.BigEndian.Uint32(k[shardLen:])
			keysProcessed++

			// progress report every 5 seconds
			if time.Since(lastReport) > 5*time.Second {
				var pct float64
				if totalRanges > 0 {
					pct = float64(rangeIdx) / float64(totalRanges) * 100
				}
				fmt.Printf("\rprocessed %d keys, range %d/%d (%.0f%%)",
					keysProcessed, rangeIdx, totalRanges, pct)
				lastReport = time.Now()
			}

			for i := 0; i+entryLen <= len(v); i += entryLen {
				var entry = v[i : i+entryLen]
				var remainder = entry[:remainderLen]
				var full = string(append(shard, remainder...))
				counts[full]++
			}
		}
		return nil
	})
	fmt.Printf("\rprocessed %d keys in %s\n", keysProcessed, time.Since(began).Round(time.Second))

	// sort by count descending, take top N
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
		fmt.Printf("%d. %s — %d transactions\n", i+1, hex.EncodeToString([]byte(a.addr)), a.count)
	}
}
