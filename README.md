# bitnsbot

[@bitnsbot](https://telegram.me/bitnsbot)

Bitcoin network events notification bot. It can send notifications on watched addresses and transactions. It also sends various information and statistics on Bitcoin network

### How to run

Prerequites: Bitcoin Core node running with RPC, REST, ZMQ notification and transactions index  enabled. Fully synced and **not** pruned

```bash
go test ./... && go build .
./bitnsbot -bot-token=... -webhook-url=http://127.0.0.1:8082/bot -listen=127.0.0.1:8082 -db=bitnsbot.db -core-url=http://127.0.0.1:8332 -core-cookie=cookie -core-rest=http://127.0.0.1:8332 -core-zmq=tcp://127.0.0.1:28332 -verbose 1
```

Another option: there are [scripts](scripts) to deploy a bot as a systemd service.  

### Useful tools

[advertise](tools/advertise) - simple command line tool which sends advertising packets with a specific IP to all nodes in Bitcoin network

dbui - simple web interface to manage bot database. Embedded into a bot itself 

### Roadmap

 - I18N
 - Daily statistics. Send every day market, transactions volume, moved coins, etc statistics
 - Improve data quality. More reliable and correct answers on addresses
