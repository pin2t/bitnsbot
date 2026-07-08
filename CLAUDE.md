# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

bitnsbot is a Bitcoin network events notification bot for Telegram. It's a single Go binary built almost entirely on the standard library — the one exception is `go.etcd.io/bbolt` (embedded KV store) for persisting watches, since the standard library has no embedded database. It receives updates via a Telegram webhook (not long polling).

## Commands

- Build: `go build ./...`
- Run: `TELEGRAM_BOT_TOKEN=<token> go run . -webhook-url http://localhost:8080/bot -api-base-url http://localhost:8081`
- Test all: `go test ./...`
- Test one: `go test -run TestWatchFlow -v` (other tests: `TestInfoFlow`, `TestMessageLogging`, `TestWatchStoreAdd`)
- Vet: `go vet ./...`

There is no Makefile, linter config, or CI in this repo — `go build`/`go vet`/`go test` are the only checks available. `gofmt -l .` will flag files as unformatted by design; see Style below before "fixing" that.

Flags (all in `main.go`): `-listen` (address this bot's webhook server binds to), `-webhook-path`, `-webhook-url` (the URL registered via `setWebhook`), `-api-base-url` (Bot API server to call), `-secret-token` (optional), `-register-webhook` (set false to skip calling `setWebhook` on startup), `-db` (path to the bbolt watches database, default `watches.db`).

## Runtime model

The bot is designed to run behind a self-hosted `telegram-bot-api` proxy (https://github.com/tdlib/telegram-bot-api) on localhost, not directly against `https://api.telegram.org`:
- `-api-base-url` defaults to `http://localhost:8081`; `telegram.go`'s `bot.call` builds every outgoing request URL from it.
- The webhook HTTP server itself is plain `http.ListenAndServe` — no TLS. The local proxy is assumed to own the real HTTPS conversation with Telegram; the hop from proxy to this bot is local/plaintext.
- Two secrets: `TELEGRAM_BOT_TOKEN` (env var, required) authenticates outbound Bot API calls. `-secret-token` (flag, optional) is passed to `setWebhook` and then checked against the `X-Telegram-Bot-Api-Secret-Token` header on every incoming request (constant-time compare in `main.go`'s webhook handler) — this is the only thing stopping an arbitrary POST to the webhook path from being treated as a real Telegram update.

## Architecture

Three source files, all `package main`:
- `telegram.go` — the Bot API client: the unexported `bot` type, `newBot`, and `call`/`send`/`setWebhook` methods, plus the wire types (`Update`, `Message`, `User`, `Chat`) decoded from incoming webhook JSON.
- `watches.go` — the `watchStore` (a single bbolt bucket named `watches`), storing one JSON-encoded `watchRecord` per watch under an auto-incrementing key (`bbolt.Bucket.NextSequence` + big-endian encoding, the standard bbolt idiom for ordered keys). No update/list/delete — only `add`, since nothing else needs it yet.
- `main.go` — flags, the HTTP webhook server, and all command handling/dispatch.

### The watches store

`/watch <arg>` persists a `watchRecord{CreatedAt, ChatID, Type, WatchID}` via the package-level `store *watchStore` (opened once in `main()` from `-db`, closed with `defer`). `watch()` in `main.go` classifies `arg` before storing it: exactly 64 hex characters is treated as `watchTypeTransaction`, anything else as `watchTypeAddress` — a heuristic, not real address/txid validation (no checksum verification), good enough to route the record but not to guarantee the input is well-formed. `store` is a plain package-level global (like the pending-argument maps below), not threaded through function parameters — consistent with how this codebase already handles cross-cutting state.

### Command dispatch and the "pending argument" pattern

`update()` in `main.go` is the single entry point for every incoming Telegram update: pull `Update.Message`, log it (`logMessage`), split it into a command and argument (`parseCommand`), then switch on the command.

Commands that take a free-text argument (`/info`, `/watch`) share one shape: called with an argument (`/info foo`) they act immediately; called bare (`/info`) they reply asking the user to send the text as a follow-up message and mark that chat "pending" for that command. The next plain-text (non-command) message from a pending chat is then treated as the missing argument.

Pending state is tracked per command with its own package-level `sync.Mutex` + `map[int64]bool` (`pendingInfoMu`/`pendingInfoChats` and `pendingWatchMu`/`pendingWatchChats`) — there is no shared/generic pending mechanism, and that's intentional: an earlier pass had the opportunity to unify this and deliberately kept it duplicated per command. Follow that precedent for any new argument-taking command rather than generalizing.

Note the two existing commands aren't even implemented at the same level of indirection as each other: `/info`'s pending logic is wrapped in named functions (`setPendingInfo`, `takePendingInfo`, `clearPendingInfo`), while `/watch`'s equivalent logic is inlined directly into `watch()` and `update()` (no wrapper functions at all). This asymmetry is the result of deliberate incremental simplification, not an oversight — match whichever neighboring pattern you're extending, don't "fix" the inconsistency by making them match.

## Style conventions specific to this repo

These diverge from typical idiomatic Go on purpose (established through explicit instruction over this repo's history) — don't "clean them up":
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
