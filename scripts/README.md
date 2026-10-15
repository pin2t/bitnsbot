# Deployment scripts

Continuous-deployment helpers for running bitnsbot on a Raspberry Pi (or any
systemd Linux host). The runtime package is **self-contained** under
`/opt/bitnsbot`:

| Path                        | What                                             |
|-----------------------------|--------------------------------------------------|
| `/opt/bitnsbot/bitnsbot`    | the binary                                        |
| `/opt/bitnsbot/bitnsbot.conf`| config (`name=value`; `chmod 600` — holds secrets)|
| `/opt/bitnsbot/bitnsbot.db` | SQLite database (created on first run)            |
| `/opt/bitnsbot/update.sh`   | copy of the auto-update script (run by the timer) |
| `/opt/bitnsbot/deploy.env`  | records `REPO_DIR` / `BUILD_USER` for updates     |

The source checkout stays where you cloned it (e.g. `/home/pi/bitnsbot`); the
scripts build there, as the checkout's owner, and only copy the binary into
`/opt`.

`install.sh` also serves the landing page in the checkout's `static/` (see
[The landing page](#the-landing-page) below), as a second service.

## Prerequisites

- Go, `git`, and `systemd` on the host; the repo cloned (default assumption:
  `/home/pi/bitnsbot`, owned by `pi`).
- `origin` reachable for `git fetch`. The public GitHub repo works over HTTPS
  with no credentials; for a private remote, give `pi` a read-only deploy key.
- A Bitcoin Core systemd unit named `bitcoind.service` (the bot's unit is
  ordered `After=bitcoind.service`). If yours has a different name, edit that
  `After=` line in `/etc/systemd/system/bitnsbot.service` — an unknown unit name
  there is simply ignored, so a wrong name costs ordering, not startup.
- Bitcoin Core itself needs two things set in `bitcoin.conf` for the bot to
  use everything it can — the transaction index and ZMQ, which with JSON-RPC is
  everything the bot asks of it:

  ```
  txindex=1                                # /info on any transaction
  zmqpubhashblock=tcp://127.0.0.1:28332    # new-block notifications
  zmqpubrawtx=tcp://127.0.0.1:28333        # mempool notifications for watches
  ```

  Neither is strictly required to start: without ZMQ the bot answers commands
  but never notifies.

## Logs

Everything — the bot, the auto-update and the landing page — logs to the
systemd journal. The bot detects it is writing to the journal (systemd's
`JOURNAL_STREAM`) and then leaves the timestamp off each line, which the journal
adds itself, and marks each line with its syslog priority, so:

```sh
journalctl -u bitnsbot -p warning   # only the bot's warnings and errors
journalctl -u bitnsbot -p err       # only its errors
```

## Usage

```sh
sudo ./scripts/install.sh        # build + test, install service + update timer, start
sudo ./scripts/uninstall.sh      # stop + remove service and update timer (keeps data)
sudo ./scripts/uninstall.sh --purge   # also delete /opt/bitnsbot and the log
```

`install.sh` builds and tests first and **installs nothing if either fails**. On
the first run it drops a config **template** at `/opt/bitnsbot/bitnsbot.conf` and
does *not* start the service — edit that file (at least `botToken`), then:

```sh
sudo systemctl start bitnsbot
```

## The landing page

`static.sh` — run by `install.sh`, and safe to run on its own or again — serves
`static/` from the checkout with **lighttpd**, as the `bitnsbot-static` service
on `127.0.0.1:8086`:

| Path                                          | What                              |
|-----------------------------------------------|-----------------------------------|
| `/etc/lighttpd/bitnsbot-static.conf`          | its own lighttpd config            |
| `/etc/systemd/system/bitnsbot-static.service` | the service                        |

```sh
sudo ./scripts/static.sh                    # (re)install just the landing page
sudo STATIC_PORT=8090 ./scripts/static.sh   # elsewhere; STATIC_BIND sets the address
sudo STATIC=0 ./scripts/install.sh          # install the bot without it
```

- **It serves the checkout itself**, so each auto-update puts the new page live
  without a restart.
- **It runs as the checkout's owner, not `www-data`**: a home directory is often
  mode 700 (Raspberry Pi OS's is), which `www-data` cannot read. The unit lets it
  read `/home` and write nowhere.
- **The stock `lighttpd.service` is masked when `static.sh` installs lighttpd**,
  since Debian's package would otherwise start it on port 80 straight away. A
  lighttpd that was already installed is left as it was. Unmask it with
  `sudo systemctl unmask lighttpd` if you want it.
- **It is local only.** Publish it the way the Mini App is published: a public
  hostname in the Cloudflare tunnel pointing at `http://127.0.0.1:8086`.
- A failure here does not undo the bot's install; `install.sh` says so and the
  bot keeps running.

`uninstall.sh` removes the service and its config, and leaves the lighttpd
package installed.

## Auto-update

`install.sh` installs `bitnsbot-update.timer`, which runs `update.sh` as the
oneshot `bitnsbot-update.service` every 5 minutes (`systemctl list-timers` shows
the next run). Each run:

1. `git fetch origin master`; if `HEAD` already equals `origin/master`, it exits
   silently (so the journal only hears of real updates).
2. Otherwise checks out `master`, fast-forwards to `origin/master`, then
   **builds and runs the full test suite**.
3. Only if both pass does it atomically swap in the new binary and
   `systemctl restart bitnsbot`. A failed build or test leaves the running
   binary untouched and logs the failure.

Its output — the build and test output included — goes to the journal, like the
bot's, with failures at priority `err`, so `journalctl -u bitnsbot-update -p err`
lists only failed updates. A timer never starts a second run of a service that
is still running, and a `flock` keeps a run by hand from overlapping one.

Earlier versions ran `update.sh` from `/etc/cron.d/bitnsbot` and appended to
`/var/log/bitnsbot-update.log`; `install.sh` removes that cron job and leaves the
old log where it is (`uninstall.sh --purge` deletes it).

Follow along with:

```sh
journalctl -u bitnsbot -f          # the service
journalctl -u bitnsbot-static -f   # the landing page
journalctl -u bitnsbot-update -f   # auto-updates
```

## Notes / limitations

- Updates only ever fast-forward `master` to `origin/master`. If the Pi's
  checkout is ahead of or has diverged from `origin/master` (e.g. you made local
  commits), the update is skipped — nothing is fast-forwarded, discarded, or
  redeployed.
- Changing these scripts themselves requires re-running `install.sh` (the timer
  runs the copy in `/opt`, not the one in the repo).
- Override defaults with env vars, e.g. `sudo BUILD_USER=someone ./scripts/install.sh`.
