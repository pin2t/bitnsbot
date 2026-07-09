package main

import "context"
import "crypto/tls"
import "crypto/x509"
import "encoding/base64"
import "fmt"
import "net/http"
import "os"
import "strings"
import "time"

import "github.com/gorilla/websocket"
import "github.com/sourcegraph/jsonrpc2"
import jsonrpc2ws "github.com/sourcegraph/jsonrpc2/websocket"

type btcdConfig struct {
	url         string
	user        string
	pass        string
	certFile    string
	insecureTLS bool
}

type btcdClient struct {
	conn *jsonrpc2.Conn
}

func dialBtcd(ctx context.Context, cfg btcdConfig, handler jsonrpc2.Handler) (*btcdClient, error) {
	var header = http.Header{}
	header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cfg.user+":"+cfg.pass)))
	var dialer = websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	if strings.HasPrefix(cfg.url, "wss://") {
		var tlsConfig, err = btcdTLSConfig(cfg)
		if err != nil { return nil, err }
		dialer.TLSClientConfig = tlsConfig
	}
	var wsConn, _, err = dialer.DialContext(ctx, cfg.url, header)
	if err != nil { return nil, err }
	var stream = jsonrpc2ws.NewObjectStream(wsConn)
	var conn = jsonrpc2.NewConn(ctx, stream, handler)
	return &btcdClient{conn: conn}, nil
}

func btcdTLSConfig(cfg btcdConfig) (*tls.Config, error) {
	if cfg.insecureTLS {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
	if cfg.certFile == "" {
		return &tls.Config{}, nil
	}
	var pem, err = os.ReadFile(cfg.certFile)
	if err != nil { return nil, err }
	var pool = x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", cfg.certFile)
	}
	return &tls.Config{RootCAs: pool}, nil
}

func (c *btcdClient) close() error {
	return c.conn.Close()
}

func (c *btcdClient) getBlockCount(ctx context.Context) (int64, error) {
	var count int64
	var err = c.conn.Call(ctx, "getblockcount", []interface{}{}, &count)
	return count, err
}

type btcdTransaction struct {
	Txid          string `json:"txid"`
	Hash          string `json:"hash"`
	Confirmations uint64 `json:"confirmations"`
	BlockHash     string `json:"blockhash"`
	Time          int64  `json:"time"`
}

func (c *btcdClient) getRawTransaction(ctx context.Context, txid string) (*btcdTransaction, error) {
	var tx btcdTransaction
	var err = c.conn.Call(ctx, "getrawtransaction", []interface{}{txid, 1}, &tx)
	if err != nil { return nil, err }
	return &tx, nil
}

type btcdOutpoint struct {
	Hash  string `json:"hash"`
	Index uint32 `json:"index"`
}

func (c *btcdClient) loadTxFilter(ctx context.Context, reload bool, addresses []string, outpoints []btcdOutpoint) error {
	if addresses == nil {
		addresses = []string{}
	}
	if outpoints == nil {
		outpoints = []btcdOutpoint{}
	}
	return c.conn.Call(ctx, "loadtxfilter", []interface{}{reload, addresses, outpoints}, nil)
}

func (c *btcdClient) notifyBlocks(ctx context.Context) error {
	return c.conn.Call(ctx, "notifyblocks", []interface{}{}, nil)
}
