# addrindex-scan

Scans the [bitnsbot](https://github.com/pin2t/bitnsbot) address index for a Bitcoin address and prints every transaction it was involved in.

## Usage

```
addrindex-scan <address> -db=<path> -core-url=<url> [-core-cookie=<path>]
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `-db` | `bitnsbot.db` | Path to the bbolt database |
| `-core-url` | | Bitcoin Core JSON-RPC URL (e.g. `http://127.0.0.1:8332`) |
| `-core-user` | | RPC username |
| `-core-pass` | | RPC password |
| `-core-cookie` | | Path to `.cookie` file (alternative to user/pass) |

### Example

```
addrindex-scan bc1qeandws6k5jqxsjn7dw08pfkgnd64l4sw6uv49u \
  -db=bitnsbot.db \
  -core-url=http://127.0.0.1:8332 \
  -core-cookie=/path/to/.cookie
```

### Output

One transaction per line:

```
2 Jan 2021 13:00: block #923456, pos 123, tx 1233456...987654, amount 123 sats, in: bc1qabc...lkjh (100 sats), out: bc1qghj...lllkj (50 sats)
```

Each line shows the transaction time, block height, position in block, shortened txid, total output amount, inputs (with their amounts), and outputs (with their amounts).

## Build

```
go build ./tools/addrindex-scan
```
