# Migrating from btcd to Bitcoin Core

Work in progress on branch `bitcoin-core`. This file is the working notes; when
the migration lands, its conclusions belong in CLAUDE.md and this file goes away.

Everything below was verified hands-on against **Bitcoin Core v31.1.0** on
regtest (`bitcoind -regtest` with `txindex=1` and the four ZMQ publishers), not
taken from documentation.

## Status

Done:
- `core.go` — the HTTP JSON-RPC client and every typed method the bot needs.
- `zmq.go` — the ZMQ subscriber, the local transaction parser, and the
  watched-script/outpoint matcher.
- `zmq_test.go` — parser tests built from real regtest transactions.
- `addrindex/` — the bespoke address history index (Core has none at all) and
  `addrindex_source.go`, its REST-based backfill source. Built, unit-tested
  against real regtest blocks, and run end-to-end against a live regtest node
  through the exported API only. See "Address indexing" below.

Not done — the bot still talks to btcd, and `btcd.go` is still the live path:
- rewriting `txInputs`, `blockFees`, `seedOutpoints` and `notifier`
- flags, startup wiring (including calling `startAddrIndex`), and `/fees`
- reworking the test fakes (they impersonate a btcd **websocket** server)
- `address()` still needs wiring to `addrindex.Lookup` (see below) — it
  currently still calls the btcd-only `addressTxs`

## What Core gives us, exactly

| bot need | btcd | Bitcoin Core |
|---|---|---|
| transport | JSON-RPC over websocket | HTTP JSON-RPC only — **no websocket** |
| auth | user/pass | user/pass or the `.cookie` file Core writes itself |
| new blocks | `blockconnected` push | ZMQ `hashblock` |
| mempool txs, filtered by address | `relevanttxaccepted` + `loadtxfilter` | **nothing** — ZMQ `rawtx` gives *every* transaction, filtering is ours |
| fee estimate | `estimatefee` → BTC/kB float | `estimatesmartfee` → `{feerate, blocks, errors}` |
| address history | `searchrawtransactions` (`--addrindex`) | **nothing at all** — see `addrindex/` |
| address UTXOs | derived from history | `scantxoutset` with `addr()` descriptors |

Same on both: `getblockcount`, `getblockhash`, `getblockheader`, `getblock`,
`getrawtransaction`, `decoderawtransaction`, `validateaddress`,
`getmempoolinfo`, `getrawmempool`.

## Findings that change the code

**`getblock` verbosity 2 already carries a per-transaction `fee`.** This deletes
`blockFees` outright — the bounded 16-way prevout fan-out exists only because
btcd made us compute fees ourselves. Verbosity 3 adds full `prevout` objects but
costs ~30× verbosity 0, and we do not need it.

**Core puts the full transactions under `tx`; btcd used `rawtx`.** This is the
exact reverse of the gotcha recorded in CLAUDE.md, so the `json:"rawtx"` tag has
to go.

**`getrawtransaction` verbosity 2 gives `fee` and per-input `prevout` — but only
for *confirmed* transactions.** A mempool transaction has no undo data, so both
fields are absent there. This matters because the watch notifier enriches
*mempool* transactions:
- confirmed → verbosity 2, one call, no fan-out (`/info <txid>`)
- unconfirmed → `getmempoolentry` gives `fees.base` and `vsize`, also one call

So the fee path gets better in both cases, just via two different calls.

**`validateaddress` returns the address's `scriptPubKey`.** This is what makes
local matching viable: a watched address is converted to its script once, and
ZMQ transactions are matched on raw script bytes. The bot never has to encode or
decode an address format itself.

**`estimatesmartfee` reports "no data" in-band.** On regtest it returns
`{"errors":["Insufficient data or no feerate found"],"blocks":0}` with HTTP 200
and no RPC error, so `/fees` must treat a missing `feerate` as unavailable rather
than waiting for a call to fail.

**ZMQ needs no broker and interoperates with pure Go.** Core `zmq_bind()`s and
the subscriber dials in — there is no intermediary process, and the roles cannot
be reversed. `github.com/go-zeromq/zmq4` (pure Go, no cgo) was verified against
Core's libzmq publisher: messages arrive as three frames, `[topic][body][sequence
number]`, the last being a per-topic counter for detecting drops.

**`getblock` verbosity 1 has a `coinbase_tx` field** holding the coinbase *input*
— so the miner tag is available without a second call, though the payout
addresses and reward still need the outputs.

## Design decisions

**Matching moved into the bot.** Losing `loadtxfilter` means the bot keeps its
own equivalent of btcd's filter: a set of watched scriptPubKeys (from
`validateaddress`) and a set of watched outpoints (seeded by `scantxoutset`, and
extended whenever a watched address is paid). `zmq.go` holds both.

**Raw transactions are parsed locally rather than sent to `decoderawtransaction`.**
On mainnet this path runs several times a second and almost nothing matches, so
an RPC round trip per transaction would be pure waste. `parseTx` reads only the
inputs' outpoints and the outputs' scripts, stopping before the witness and
locktime. Its tests are built from real regtest transactions — a segwit spend, a
three-output send and a coinbase — asserted against what Core itself reported.

**Address history is restored by a bespoke index — see below.** Nothing in Core
replaces `searchrawtransactions`, and Fulcrum was ruled out as a dependency (a
second service to run and keep in sync). `addrindex/` reimplements the idea
directly against the bot's own bbolt file.

Spend detection needed no index at all: it needs current UTXOs, not history, and
one `scantxoutset` call covers every watched address at once.

## Address indexing

Three existing implementations were read for their on-disk design before writing
ours, not guessed from memory:

- **btcd's `addrindex.go`** (read locally): a 21-byte address key → a doubling
  "level" merge scheme storing 12-byte `{block ID, offset, length}` pointers into
  btcd's *own* raw block files. Compact, but only works because btcd owns its
  block storage — we don't, we talk to Core over HTTP.
- **Fulcrum**: RocksDB, scripthash-keyed, measured at **133 GB for BTC mainnet
  in August 2023**. Ruled out as a dependency anyway (a second service to run).
- **electrs' current backend, `bindex-rs`** (cloned and read — by electrs' own
  author, built for Core 31): the best reference. It hashes each `scriptPubKey`,
  truncates to an **8-byte prefix**, pairs it with a 4-byte global tx number, and
  stores that 12-byte blob as a RocksDB **key with an empty value** — one row per
  touch, for both the funding (output) and spending (prevout) sides. Reports
  ~10% of blockchain size.

**The load-bearing discovery**: `bindex` doesn't fetch the spending side through
the JSON RPC. It calls Core's REST endpoint `/rest/spenttxouts/<hash>.bin` —
binary, no JSON. Measured against the live mainnet node: raw block + spent
outputs is **1.95 MB in 28 ms**, where `getblock` verbosity 3 for the same block
is **13.7 MB** of JSON. This is what makes a full historical backfill tractable.

### The layout, and the one that failed first

bbolt is a B+tree: no compaction, no compression, no prefix sharing across keys,
and a per-element page cost. bindex's row-per-touch scheme relies on all three of
those RocksDB properties, so it cannot be ported directly.

**First attempt — one key per address, value = append-only list of its touches**,
on the theory that per-key overhead would amortize across repeated touches.
Measured against real mainnet blocks, it did not. From 25-block windows:

| window | touches | file | bytes/touch |
|---|---|---|---|
| 958800 | 348,724 | 16.8 MB | 48 |
| 800000 | 320,593 | 34.1 MB | 107 |
| 500000 | 272,887 | 33.7 MB | 123 |

Solving `cost = addresses×a + touches×6` gave **~133 bytes per distinct address**
against 30 bytes of content, projecting to **150-250 GB**. The premise was simply
false — measured over 25 recent blocks:

```
addresses touched once:      71.0%
addresses touched twice:     25.2%
addresses touched 3+ times:   3.8%
```

96% of addresses are touched at most twice, so there is nothing to amortize.

**Shipped layout — sharded.** The key is `shard(2 bytes of the script hash) +
rangeIndex(4)`; the value is the packed run of every touch in that shard and
block range, each `remainder(6) + heightOffset(2) + txIndex(2)` = 10 bytes.
`rangeBlocks` is 1000, which keeps the height offset inside a uint16 and matches
the backfill chunk size. Shard-first key ordering puts all of one shard's ranges
together, so a lookup is one contiguous cursor scan.

This buys two things the first layout could not have:
- **Writes are append-only.** Each (shard, range) value is written once, when
  that chunk is indexed, and never rewritten. The first layout rewrote an
  address's entire value on every touch, which is why it needed a write-side
  history cap to avoid quadratic rewrites on exchange-hot addresses. That cap is
  gone — history is complete on disk, and only `Lookup` stops early (maxLookup),
  which is what flags the "10000+" case to the user.
- **Per-key overhead stops mattering**: ~65k keys per range instead of one per
  address.

### Measured result

900 real mainnet blocks (958000-958900), batched into one merge exactly as the
backfill does:

```
12,313,387 touches   file 223.48 MB   18.15 bytes/touch   26s
```

Projecting to the chain's ~1.4 billion transactions at the measured (already
deduplicated) 3.34 touches/tx gives **roughly 85 GB**; using the raw
outputs+inputs count of 6.2e9 touches as a pessimistic bound gives 113 GB.
Either way it is inside budget.

Two measurement traps worth recording, since both produced confidently wrong
numbers before being caught:
- **A 25-block sample is meaningless here.** Sharded values only reach their
  steady-state size once a whole range is in them; at 25 blocks every window
  reported *exactly* 16.78 MB regardless of touch count, because the cost was
  entirely per-key overhead against nearly-empty values.
- **Merging per block instead of per chunk measures the wrong design.** Calling
  `merge` once per block rewrites all 65k keys every block and gave 30.89
  bytes/touch (192 GB) and 113s; the batched merge the backfill actually performs
  gave 18.15 bytes/touch and 26s.

### Tuning lever

`rangeBlocks` trades index size against lookup cost and backfill memory: larger
ranges mean fewer, bigger values (less per-key overhead, better page fill) but
more memory held per chunk and more data read per lookup. 1000 was not tuned
beyond confirming it lands in budget.

## Remaining work, in order

1. `txInputs` → verbosity 2 prevouts for confirmed, `getmempoolentry` for mempool.
2. Delete `blockFees`; take `fee` from `getblock` verbosity 2 in `computeBlockInfo`.
3. `seedOutpoints` → `scanTxOutSet`; `watchCmd` also registers the scriptPubKey.
4. Replace `notifier` with `startZMQ`; delete `loadTxFilter`/`notifyBlocks` and
   the reconnection supervisor (HTTP is stateless — there is no connection to
   reapply state to).
5. `/fees` → `estimateSmartFee`, treating a zero feerate as unavailable.
6. `/mempool` → `fees.base` instead of btcd's flat `Fee`.
7. `address()` → resolve the address to a scriptPubKey (`validateaddress`), call
   `addrindex.Lookup`, resolve each distinct touched height to real txids via
   `getblock` verbosity 1 (cached), then fetch and sum the same way
   `addressStats` already does. Falls back to today's `Activity: unavailable`
   only if the index isn't built yet or Core's REST isn't enabled.
8. Flags: `-core-url`, `-core-user`, `-core-pass`, `-core-cookie`, `-core-zmq`,
   `-core-rest` (for `startAddrIndex`), replacing the five `-btcd-*` ones.
9. Wire `addrindex.Init(db)` into `openDB` and call `startAddrIndex` from
   `main()`, alongside the rest of the Core startup sequence.
10. Test fakes: replace the websocket btcd server with an HTTP JSON-RPC one.
11. Once a real mainnet slice has been measured (see "Address indexing" above),
    decide and record the actual genesis-to-tip disk/time cost before
    recommending a full unattended backfill.
