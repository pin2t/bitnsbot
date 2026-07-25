# advertise

Sends an `addr` message to Bitcoin P2P peers, announcing a node address so it propagates through the gossip network. Performs the version/verack handshake for each peer and then sends the address.

## Usage

```sh
# use btcd's peers.json
go run ./tools/advertise/ -peers /path/to/peers.json

# use Bitcoin Core's peers.dat
go run ./tools/advertise/ -corepeers ~/.bitcoin/peers.dat

# combine both (deduplicated)
go run ./tools/advertise/ -peers /path/to/peers.json -corepeers ~/.bitcoin/peers.dat

# limit to 100 peers, only those btcd has reached before
go run ./tools/advertise/ -peers /path/to/peers.json -limit 100 -live
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-peers` | *(empty)* | Path to btcd's peers.json |
| `-corepeers` | *(empty)* | Path to Bitcoin Core's peers.dat |
| `-advertise` | `5.181.105.56:8333` | Address to announce to every peer |
| `-workers` | `64` | Concurrent connections |
| `-timeout` | `10s` | Dial and I/O timeout per peer |
| `-limit` | `0` (all) | Maximum number of peers to contact |
| `-live` | `false` | Only contact peers btcd has successfully connected to (peers.json only) |
| `-verbose` | `0` | Log level: 0=summary, 1=+per-peer |

At least one of `-peers` or `-corepeers` is required. Only IPv4 peers are contacted — IPv6 and Tor addresses are skipped.
