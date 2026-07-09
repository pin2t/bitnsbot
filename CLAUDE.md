# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

bitnsbot is a Bitcoin network events notification bot for Telegram. It's a single Go binary built almost entirely on the standard library — the exceptions are `go.etcd.io/bbolt` (embedded KV store, for persisting watches) and, for talking to a `btcd` node, `github.com/gorilla/websocket` + `github.com/sourcegraph/jsonrpc2` (no other option exists in the standard library for any of these). It receives updates via a Telegram webhook (not long polling).

## Commands

- Build: `go build ./...`
- Run: `TELEGRAM_BOT_TOKEN=<token> go run . -webhook-url http://localhost:8080/bot -api-base-url http://localhost:8081`
- Test all: `go test ./...`
- Test one: `go test -run TestWatchFlow -v` (other tests: `TestInfoFlow`, `TestMessageLogging`, `TestWatchStoreAdd`, `TestBtcd*`)
- Vet: `go vet ./...`

There is no Makefile or linter config in this repo — `go build`/`go vet`/`go test` are the only checks available (and are exactly what `.github/workflows/ci.yml` runs on every pull request, on Go 1.26). `gofmt -l .` will flag files as unformatted by design; see Style below before "fixing" that.

Flags (all in `main.go`): `-listen` (address this bot's webhook server binds to), `-webhook-path`, `-webhook-url` (the URL registered via `setWebhook`), `-api-base-url` (Bot API server to call), `-secret-token` (optional), `-register-webhook` (set false to skip calling `setWebhook` on startup), `-db` (path to the bbolt watches database, default `watches.db`), `-btcd-url`/`-btcd-user`/`-btcd-pass`/`-btcd-cert`/`-btcd-insecure-tls` (btcd RPC connection; leaving `-btcd-url` empty skips connecting to btcd entirely).

## Runtime model

The bot is designed to run behind a self-hosted `telegram-bot-api` proxy (https://github.com/tdlib/telegram-bot-api) on localhost, not directly against `https://api.telegram.org`:
- `-api-base-url` defaults to `http://localhost:8081`; `telegram.go`'s `bot.call` builds every outgoing request URL from it.
- The webhook HTTP server itself is plain `http.ListenAndServe` — no TLS. The local proxy is assumed to own the real HTTPS conversation with Telegram; the hop from proxy to this bot is local/plaintext.
- Two secrets: `TELEGRAM_BOT_TOKEN` (env var, required) authenticates outbound Bot API calls. `-secret-token` (flag, optional) is passed to `setWebhook` and then checked against the `X-Telegram-Bot-Api-Secret-Token` header on every incoming request (constant-time compare in `main.go`'s webhook handler) — this is the only thing stopping an arbitrary POST to the webhook path from being treated as a real Telegram update.

## Architecture

Four source files, all `package main`:
- `telegram.go` — the Bot API client: the unexported `bot` type, `newBot`, and `call`/`send`/`setWebhook` methods, plus the wire types (`Update`, `Message`, `User`, `Chat`) decoded from incoming webhook JSON.
- `watches.go` — the `watchStore` (a single bbolt bucket named `watches`), storing one JSON-encoded `watchRecord` per watch under an auto-incrementing key (`bbolt.Bucket.NextSequence` + big-endian encoding, the standard bbolt idiom for ordered keys). No update/list/delete — only `add`, since nothing else needs it yet.
- `btcd.go` — a `btcdClient` for talking to a `btcd` node's JSON-RPC-over-websocket API (see below).
- `main.go` — flags, the HTTP webhook server, and all command handling/dispatch.

### The btcd RPC client

`dialBtcd(ctx, btcdConfig{url, user, pass, certFile, insecureTLS}, handler)` dials `btcdConfig.url` with `github.com/gorilla/websocket` (HTTP Basic Auth header for credentials, matching btcd's preferred auth method over the websocket-only `authenticate` RPC command), then wraps the resulting `*websocket.Conn` in `github.com/sourcegraph/jsonrpc2/websocket`'s `ObjectStream` and hands that to `jsonrpc2.NewConn` to get a `*jsonrpc2.Conn`. TLS is only configured when `url` starts with `wss://` (production btcd is TLS-by-default with a self-signed `rpc.cert`; local regtest/dev nodes are commonly run with `--notls` and a plain `ws://` URL, which is what this repo's manual testing used).

btcd/bitcoind's JSON-RPC dialect predates JSON-RPC 2.0 and always uses positional (array) `params`, never named/object params — every method call in `btcd.go` passes `[]interface{}{...}` as the params argument for this reason. Confirmed against btcd source (`btcjson.RPCVersion.IsValid()` accepts both `"1.0"` and `"2.0"`) that sending `"jsonrpc":"2.0"` (what `sourcegraph/jsonrpc2` always sends) doesn't get rejected.

Only a handful of RPC methods are wrapped as typed methods so far (`getBlockCount`, `getRawTransaction`, `getBlockHash`, `getBlockHeader`, `validateAddress`, `searchRawTransactions`, `loadTxFilter`, `notifyBlocks`) — enough to cover `/watch` and `/info`'s needs, not the full btcd API surface. Add more the same way: a thin method on `btcdClient` that calls `c.conn.Call(ctx, "methodname", []interface{}{...}, &result)`.

Server-pushed notifications (e.g. `blockconnected`, `relevanttxaccepted` — see `/Users/pin/code/btcd/docs/json_rpc_api.md` for the full list) are delivered to whatever `jsonrpc2.Handler` is passed into `dialBtcd`.

`main()` dials btcd (if `-btcd-url` is set; empty means skip it entirely) right after opening the watches store and before registering the Telegram webhook or starting to listen — the package-level `var btcd *btcdClient` holds the connection, `defer btcd.close()` on shutdown, and a dial failure is fatal (matches how `store`'s open failure is handled). The handler passed in is `btcdNotificationLogger`, which just logs every notification (`log.Printf("btcd notification: ...")`) — nothing yet matches a notification against a specific watch or sends a Telegram message for it; that logic doesn't exist.

`btcd_test.go` fakes the btcd server with an in-process `httptest.Server` + `websocket.Upgrader` (same philosophy as the Telegram tests — no real btcd node needed to run `go test`). It was additionally verified once, manually, against a real locally-built `btcd --regtest --notls` node; that was throwaway verification, not something reflected in the checked-in tests.

### The /info lookup

`info()` in `main.go` classifies its (non-empty) argument into exactly one of three kinds, checked **in this order**: transaction (`isTxid`: exactly 64 hex characters), then block height (parses as a non-negative base-10 integer via `strconv.ParseInt`), then address (the catch-all). The order matters and is not arbitrary: a 64-character string of all decimal digits (e.g. all zeros) satisfies *both* the txid shape and `ParseInt`, since decimal digits are a subset of hex digits — checking txid first is what disambiguates it correctly. This was a real bug caught by manual testing against a live regtest node, not a hypothetical; don't reorder these checks without re-verifying against a real node.

Each of the three branches calls btcd directly and replies with a plain-text (no Markdown/HTML parse_mode) multi-line message: transaction via `getRawTransaction` (reports confirmation status and the summed `Vout[].Value` as the amount — unconfirmed mempool transactions skip the block/time fields entirely rather than showing zero values), block via `getBlockHash` + `getBlockHeader`, and address via `validateAddress` (type derived from `IsWitness`/`IsScript`) plus a best-effort `searchRawTransactions` call for recent activity. The `searchRawTransactions` call requires btcd's `--addrindex` flag; if it errors for any reason (index disabled, still catching up, etc.) `info()` doesn't fail the whole reply, it just reports activity as unavailable — this is deliberate graceful degradation, not a TODO to fix by making it fail loudly.

If the package-level `btcd` is `nil` (not configured via `-btcd-url`), `info()` replies with a fixed "not configured" message instead of attempting any of the above — every branch depends on btcd, so this is checked once up front.

`isTxid` is shared between `info()` and `watch()`'s existing classification (it's the one piece of `/info`'s classification logic used at more than one call site, so unlike everything else here it's a real extracted helper, not inlined).

### The watches store

`/watch <arg>` persists a `watchRecord{CreatedAt, ChatID, Type, WatchID}` via the package-level `store *watchStore` (opened once in `main()` from `-db`, closed with `defer`). `watch()` in `main.go` classifies `arg` before storing it: exactly 64 hex characters is treated as `watchTypeTransaction`, anything else as `watchTypeAddress` — a heuristic, not real address/txid validation (no checksum verification), good enough to route the record but not to guarantee the input is well-formed. `store` is a plain package-level global (like the pending-argument maps below), not threaded through function parameters — consistent with how this codebase already handles cross-cutting state.

### Command dispatch and the "pending argument" pattern

`update()` in `main.go` is the single entry point for every incoming Telegram update: pull `Update.Message`, log it (`logMessage`), split it into a command and argument (`parseCommand`), then switch on the command.

Commands that take a free-text argument (`/info`, `/watch`) share one shape: called with an argument (`/info foo`) they act immediately; called bare (`/info`) they reply asking the user to send the text as a follow-up message and mark that chat "pending" for that command. The next plain-text (non-command) message from a pending chat is then treated as the missing argument.

Pending state is tracked per command with its own package-level `sync.Mutex` + `map[int64]bool` (`pendingInfoMu`/`pendingInfoChats` and `pendingWatchMu`/`pendingWatchChats`) — there is no shared/generic pending mechanism, and that's intentional: an earlier pass had the opportunity to unify this and deliberately kept it duplicated per command. Follow that precedent for any new argument-taking command rather than generalizing.

Both commands are implemented the same way, one-word handler names (`info`, `watch`) with no wrapper functions around their pending-state maps: the lock/set-or-delete/unlock logic is inlined directly into `info()`/`watch()` (setting pending) and into `update()`'s `case ""` (checking and clearing pending). There was an earlier pass where `/info` kept named helper functions (`setPendingInfo`, `takePendingInfo`, `clearPendingInfo`) while `/watch` was already inlined — that asymmetry was deliberately removed by request, so treat the current inlined-everywhere shape as the pattern to extend, not something to re-wrap in helpers.

## Style conventions specific to this repo

These diverge from typical idiomatic Go on purpose (established through explicit instruction over this repo's history) — don't "clean them up":
- Indent with 4 spaces, never tabs — this includes running `gofmt`/`goimports` with a post-process step (or an editor setting) to expand tabs, since Go's own tooling always indents with tabs by default.
- Prefer unexported identifiers even for the main API (`bot`/`newBot`/`send`, not `Bot`/`NewBot`/`SendMessage`) — nothing outside `package main` consumes this code.
- One `import "x"` line per import, never a grouped `import (...)` block.
- `var x = expr()` instead of `x := expr()` wherever Go's grammar allows it. It doesn't allow `var` in an `if`/`for` initializer, and a `var` can't redeclare a name already in scope the way `:=` can (e.g. `req, err := ...` when `err` already exists) — those cases keep `:=`.
- No blank lines inside function bodies; blank lines appear only between top-level declarations.
- Minimal comments: none explaining *what* code does, only rare ones for non-obvious *why*.
- Single-line `if err != nil { return nil, err }` where it fits on one line.
- Avoid introducing a helper function for logic used at only one call site — inline it instead.

## Testing approach

Tests (`flow_test.go`, `log_test.go`) never hit real Telegram or a real `telegram-bot-api` proxy. They spin up an `httptest.Server` standing in for the Bot API, build a `bot` pointed at it (`newBot("TESTTOKEN", server.URL)`), and drive behavior by calling `update()` directly with hand-built `Update` values — then assert on the outgoing `sendMessage` request bodies the fake server captured (and, in `log_test.go`, on `log` package output captured via `log.SetOutput`).

Any test that exercises `/watch` must set the package-level `store` first (e.g. `store, _ = openWatchStore(filepath.Join(t.TempDir(), "watches.db"))`) since `watch()` dereferences it unconditionally — there's no nil check. `watches_test.go` tests `watchStore` directly: it opens a temp bbolt file, calls `add`, then reads the raw bucket back via `db.View`/`ForEach` to assert on the stored JSON rather than adding a `get`/`list` method to production code just for testability.
