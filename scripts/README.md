# Deployment scripts

Continuous-deployment helpers for running bitnsbot on a Raspberry Pi (or any
systemd Linux host). The runtime package is **self-contained** under
`/opt/bitnsbot`:

| Path                        | What                                             |
|-----------------------------|--------------------------------------------------|
| `/opt/bitnsbot/bitnsbot`    | the binary                                        |
| `/opt/bitnsbot/bitnsbot.conf`| config (`name=value`; `chmod 600` — holds secrets)|
| `/opt/bitnsbot/bitnsbot.db` | bbolt database (created on first run)             |
| `/opt/bitnsbot/update.sh`   | copy of the auto-update script (run by cron)      |
| `/opt/bitnsbot/deploy.env`  | records `REPO_DIR` / `BUILD_USER` for updates     |

The source checkout stays where you cloned it (e.g. `/home/pi/bitnsbot`); the
scripts build there, as the checkout's owner, and only copy the binary into
`/opt`.

## Prerequisites

- Go, `git`, and `systemd` on the host; the repo cloned (default assumption:
  `/home/pi/bitnsbot`, owned by `pi`).
- `origin` reachable for `git fetch`. The public GitHub repo works over HTTPS
  with no credentials; for a private remote, give `pi` a read-only deploy key.
- A Bitcoin Core systemd unit named `bitcoind.service` (the bot's unit is
  ordered `After=bitcoind.service`). If yours has a different name, edit that
  `After=` line in `/etc/systemd/system/bitnsbot.service` — an unknown unit name
  there is simply ignored, so a wrong name costs ordering, not startup.
- Bitcoin Core itself needs three things set in `bitcoin.conf` for the bot to
  use everything it can:

  ```
  txindex=1                                # /info on any transaction
  rest=1                                   # the address index is built over REST
  zmqpubhashblock=tcp://127.0.0.1:28332    # new-block notifications
  zmqpubrawtx=tcp://127.0.0.1:28333        # mempool notifications for watches
  ```

  None are strictly required to start: without ZMQ the bot answers commands but
  never notifies, and without `rest=1` address history stays unavailable.

## Usage

```sh
sudo ./scripts/install.sh        # build + test, install service + cron, start
sudo ./scripts/uninstall.sh      # stop + remove service and cron (keeps data)
sudo ./scripts/uninstall.sh --purge   # also delete /opt/bitnsbot and the log
```

`install.sh` builds and tests first and **installs nothing if either fails**. On
the first run it drops a config **template** at `/opt/bitnsbot/bitnsbot.conf` and
does *not* start the service — edit that file (at least `bot-token`), then:

```sh
sudo systemctl start bitnsbot
```

## Auto-update

`install.sh` schedules `/etc/cron.d/bitnsbot` to run `update.sh` every 5 minutes.
Each run:

1. `git fetch origin master`; if `HEAD` already equals `origin/master`, it exits
   silently (so the cron log only grows on real updates).
2. Otherwise checks out `master`, fast-forwards to `origin/master`, then
   **builds and runs the full test suite**.
3. Only if both pass does it atomically swap in the new binary and
   `systemctl restart bitnsbot`. A failed build or test leaves the running
   binary untouched and logs the failure.

A `flock` guards against overlapping runs (a build+test can take longer than the
5-minute interval). Logs go to `/var/log/bitnsbot-update.log`.

Follow along with:

```sh
journalctl -u bitnsbot -f          # the service
tail -f /var/log/bitnsbot-update.log   # auto-updates
```

## Notes / limitations

- Updates only ever fast-forward `master` to `origin/master`. If the Pi's
  checkout is ahead of or has diverged from `origin/master` (e.g. you made local
  commits), the update is skipped — nothing is fast-forwarded, discarded, or
  redeployed.
- Changing these scripts themselves requires re-running `install.sh` (cron runs
  the copy in `/opt`, not the one in the repo).
- Override defaults with env vars, e.g. `sudo BUILD_USER=someone ./scripts/install.sh`.
