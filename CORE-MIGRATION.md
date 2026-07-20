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
  "level" merge scheme (level 0 holds 8 entries, each level double the last)
  storing 12-byte `{block ID, offset, length}` pointers into btcd's *own* raw
  block files. Compact, but only works because btcd owns its block storage — we
  don't, we talk to Core over HTTP.
- **Fulcrum**: RocksDB, scripthash-keyed. Its real measured footprint was
  **133 GB for BTC mainnet in August 2023** (chain was ~490 GB then) — bigger
  than the ratio below because it also caches full UTXO details to serve
  Electrum clients without round-tripping to Core, which we don't need.
- **electrs' current backend, `bindex-rs`** (cloned and read — actively
  maintained by electrs' own author, built specifically for Core 31, i.e. what's
  running here): the best reference. `bindex-lib/src/index/scripthash.rs` hashes
  each `scriptPubKey` (not the address string — no per-type discrimination
  needed, since whole-script hashes don't collide across script types the way a
  bare hash160 payload can) with SHA-256, truncates to an **8-byte prefix**,
  pairs it with a **4-byte global tx number**, and stores that 12-byte blob as a
  RocksDB **key with an empty value** — one row per touch, for both funding
  (output) and spending (input, via the prevout's script) sides. Reports
  **~10% of blockchain size** in practice (42 GB / 336 GB, Aug 2023).

**The load-bearing discovery**: `bindex` doesn't fetch the spending side through
`getrawtransaction`/`getblock` verbosity 3 (the ~30×-slower JSON path). It calls
Core's REST endpoint `/rest/spenttxouts/<hash>.bin` — binary, no JSON, no
authentication (Core's REST interface is independent of RPC credentials; the
only gate is whether `-rest=1` is set at all). Verified directly against a live
Core v31.1.0 node: a 34-byte response that decoded byte-for-byte identical to
what `getblock` verbosity 3 reports for the same input's `prevout`. Combined with
`/rest/block/<hash>.bin` for the block itself, a full historical build needs no
JSON encoding at all — this is what makes a genesis-to-tip build tractable in
wall-clock time, not just in disk space.

**Why the on-disk layout isn't a straight port of `bindex`'s.** RocksDB is an
LSM tree: it compacts, and it compresses (Zstd here), so billions of tiny
12-byte keys are cheap. bbolt is a B+tree: no compaction, no compression, and
every key pays a page-slot overhead regardless of how small its value is. A
literal port — one bbolt key per touch — would pay that per-key overhead
hundreds of millions of times over, for no benefit bbolt can turn into disk
savings the way RocksDB does. So the layout is inverted: **one bbolt key per
address**, whose value is an **append-only list of that address's touches**.
This amortizes the per-key overhead across every touch of an address instead of
paying it per touch — the same insight behind btcd's own leveled scheme, just
implemented as a single unbounded append instead of a doubling merge, because
bbolt (unlike btcd's custom ffldb) natively supports values larger than a page.

A touch is `height(4) + txIndex(2)` = 6 bytes — not `bindex`'s 4-byte global tx
number — because that removes the need for a second txNum→txid table entirely:
resolving a touch to a real txid is one `getblock(height, 1)` call, made lazily,
only when a human actually reads an address's history (rare), and cached the
same way block lookups already are. The tradeoff is explicit: `bindex` pays a
fixed cost once per transaction to avoid ever re-deriving a txid; this indexer
pays nothing at build time and a cheap, cacheable RPC call at read time.

Per-address history is capped (`maxTouches`, 10000, mirroring the old
`addrTxLimit`) so one exchange-hot address can't force unbounded rewrites of its
own growing value.

**What's still unverified**: the real bbolt disk footprint for a genesis-to-tip
build. bbolt's page-fill and free-list behavior under this access pattern has
enough moving parts that a paper estimate wouldn't be trustworthy to better than
roughly 2×. `bindex`'s own ~10% ratio, applied to today's ~753 GB chain, would
land around 75 GB — inside the 100 GB budget — but that's RocksDB's number, not
bbolt's. The plan is to measure, not guess: run the backfill against a real,
bounded slice of mainnet through the Pi node once it's available, get an actual
bytes-per-block figure, and extrapolate before recommending a full unattended
build.

Verified end-to-end against a live regtest node (through `addrindex.StartBackfill`
and `addrindex.Lookup` only — the same API a real deployment would use): 104
blocks backfilled via REST in 200ms, and a known address's history came back
exactly right — funded at height 102, spent at height 103. `addrindex_test.go`
also covers chunk-boundary flushing and fetch-failure retry (same reasoning as
the miners collector: a failed block must not advance the cursor past it, or
it's dropped from the index for good) with a fake `Source`; `build_test.go`
pins the block/spent-outputs parsers against real regtest blocks — one with a
single spend, one with two chained off it — checked byte-for-byte against what
Core's own `getblock` verbosity 3 reports.

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
