// Command addrindex-scan scans the addrindex for a Bitcoin address and prints
// every transaction it was involved in, with amounts, inputs and outputs.
//
// Usage:
//
//	addrindex-scan bc1q... -db=bitnsbot.db -core-url=http://127.0.0.1:8332
package main

import "bytes"
import "context"
import "encoding/hex"
import "encoding/json"
import "flag"
import "fmt"
import "net/http"
import "os"
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
var totalsOnly = flag.Bool("totals", false, "only print totals, not individual transactions")

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
		"jsonrpc": "1.0", "id": "addrindex-scan", "method": method, "params": params,
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

func (c *rpcClient) validateAddress(ctx context.Context, addr string) (string, error) {
	var info struct {
		IsValid      bool   `json:"isvalid"`
		ScriptPubKey string `json:"scriptPubKey"`
	}
	if err := c.call(ctx, "validateaddress", []interface{}{addr}, &info); err != nil {
		return "", err
	}
	if !info.IsValid {
		return "", fmt.Errorf("%s is not a valid Bitcoin address", addr)
	}
	return info.ScriptPubKey, nil
}

func (c *rpcClient) getBlockHash(ctx context.Context, height int64) (string, error) {
	var hash string
	var err = c.call(ctx, "getblockhash", []interface{}{height}, &hash)
	return hash, err
}

func (c *rpcClient) getBlockTxids(ctx context.Context, hash string) ([]string, error) {
	var blk struct {
		Tx []string `json:"tx"`
	}
	if err := c.call(ctx, "getblock", []interface{}{hash, 1}, &blk); err != nil {
		return nil, err
	}
	return blk.Tx, nil
}

type txDetail struct {
	Txid  string
	Time  int64
	Fee   float64
	Vin   []txIn
	Vout  []txOut
}

type txIn struct {
	Address string
	Amount  float64
}

type txOut struct {
	Address string
	Amount  float64
}

func (c *rpcClient) getTransaction(ctx context.Context, txid string) (*txDetail, error) {
	var raw struct {
		Txid string `json:"txid"`
		Time int64  `json:"time"`
		Fee  float64 `json:"fee"`
		Vin  []struct {
			Coinbase string `json:"coinbase"`
			PrevOut  *struct {
				Value        float64 `json:"value"`
				ScriptPubKey struct {
					Address string `json:"address"`
				} `json:"scriptPubKey"`
			} `json:"prevout"`
		} `json:"vin"`
		Vout []struct {
			Value        float64 `json:"value"`
			ScriptPubKey struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	if err := c.call(ctx, "getrawtransaction", []interface{}{txid, 2}, &raw); err != nil {
		return nil, err
	}
	var tx = &txDetail{Txid: raw.Txid, Time: raw.Time, Fee: raw.Fee}
	for _, v := range raw.Vin {
		if v.Coinbase != "" {
			tx.Vin = append(tx.Vin, txIn{Address: "coinbase"})
		} else if v.PrevOut != nil {
			tx.Vin = append(tx.Vin, txIn{Address: v.PrevOut.ScriptPubKey.Address, Amount: v.PrevOut.Value})
		}
	}
	for _, v := range raw.Vout {
		tx.Vout = append(tx.Vout, txOut{Address: v.ScriptPubKey.Address, Amount: v.Value})
	}
	return tx, nil
}

func sats(v float64) string {
	return fmt.Sprintf("%.0f sats", v*1e8)
}

func short(s string) string {
	if len(s) > 16 { return s[:8] + "..." + s[len(s)-8:] }
	return s
}

func btc(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.8f", v), "0"), ".")
}

func main() {
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: addrindex-scan <address> [-db=file] [-core-url=url]")
		os.Exit(1)
	}
	var address = flag.Arg(0)

	var d, err = bbolt.Open(*dbPath, 0600, nil)
	if err != nil {
		logging.Fatal("open database: %v", err)
	}
	defer d.Close()
	if err := addrindex.Init(d); err != nil {
		logging.Fatal("init addrindex: %v", err)
	}

	var rpc *rpcClient
	if *coreURL != "" {
		rpc, err = newRPCClient(*coreURL, *coreUser, *corePass, *coreCookie)
		if err != nil {
			logging.Fatal("RPC client: %v", err)
		}
	}

	var ctx = context.Background()
	var scriptHex string
	if rpc != nil {
		scriptHex, err = rpc.validateAddress(ctx, address)
		if err != nil {
			logging.Fatal("validate address: %v", err)
		}
	} else {
		logging.Fatal("Bitcoin Core RPC (-core-url) is required to resolve the address")
	}
	var script, _ = hex.DecodeString(scriptHex)
	var touches, capped = addrindex.Lookup(script, 1000000000)

	if len(touches) == 0 {
		fmt.Println("No transactions found for", address)
		return
	}
	if capped {
		fmt.Fprintf(os.Stderr, "warning: too many touches, showing the oldest %d\n", len(touches))
	}

	if _, ok := addrindex.Cursor(); !ok {
		fmt.Println("Address index is still building — results may be partial.")
	}

	var totalReceived, totalSent float64
	var txCount int
	for _, t := range touches {
		var hash, err = rpc.getBlockHash(ctx, int64(t.Height))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  block %d: %v\n", t.Height, err)
			continue
		}
		var txids, txErr = rpc.getBlockTxids(ctx, hash)
		if txErr != nil {
			fmt.Fprintf(os.Stderr, "  block %d txids: %v\n", t.Height, txErr)
			continue
		}
		if int(t.TxIndex) >= len(txids) {
			fmt.Fprintf(os.Stderr, "  block %d: tx index %d out of range (max %d)\n", t.Height, t.TxIndex, len(txids)-1)
			continue
		}
		var txid = txids[t.TxIndex]
		var tx, txErr2 = rpc.getTransaction(ctx, txid)
		if txErr2 != nil {
			fmt.Fprintf(os.Stderr, "  block %d tx %s: %v\n", t.Height, short(txid), txErr2)
			continue
		}
		txCount++
		// track amounts for this address
		for _, v := range tx.Vin {
			if v.Address == address {
				totalSent += v.Amount
			}
		}
		for _, v := range tx.Vout {
			if v.Address == address {
				totalReceived += v.Amount
			}
		}
		if *totalsOnly {
			continue
		}
		// build the output line
		var tm = time.Unix(tx.Time, 0).UTC().Format("2 Jan 2006 15:04")
		var inParts, outParts []string
		for _, v := range tx.Vin {
			inParts = append(inParts, fmt.Sprintf("%s (%s)", short(v.Address), sats(v.Amount)))
		}
		var outTotal float64
		for _, v := range tx.Vout {
			outTotal += v.Amount
			outParts = append(outParts, fmt.Sprintf("%s (%s)", short(v.Address), sats(v.Amount)))
		}
		fmt.Printf("%s: block #%d, pos %d, tx %s, amount %s",
			tm, t.Height, t.TxIndex, short(txid), sats(outTotal))
		if len(inParts) > 0 {
			fmt.Printf(", in: %s", strings.Join(inParts, ", "))
		}
		if len(outParts) > 0 {
			fmt.Printf(", out: %s", strings.Join(outParts, ", "))
		}
		fmt.Println()
	}
	// print totals
	fmt.Printf("\n%d transactions\n", txCount)
	fmt.Printf("received: %s BTC\n", btc(totalReceived))
	fmt.Printf("sent:     %s BTC\n", btc(totalSent))
	fmt.Printf("balance:  %s BTC\n", btc(totalReceived-totalSent))
}
