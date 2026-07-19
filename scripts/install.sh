#!/usr/bin/env bash
# install.sh — build + test bitnsbot from this checkout, install it under
# /opt/bitnsbot as a systemd service (ordered after btcd), and schedule the
# auto-update cron job.
#
#   Run from the repo:  sudo ./scripts/install.sh
#
# Layout it creates (self-contained under /opt/bitnsbot):
#   /opt/bitnsbot/bitnsbot        the binary
#   /opt/bitnsbot/bitnsbot.cfg    config (name=value; created as a template if absent)
#   /opt/bitnsbot/bitnsbot.db     bbolt database (created by the service on first run)
#   /opt/bitnsbot/update.sh       copy of the auto-update script (run by cron)
#   /opt/bitnsbot/deploy.env      records REPO_DIR and BUILD_USER for update.sh
set -euo pipefail

PREFIX=/opt/bitnsbot
SERVICE=bitnsbot
BINARY=$PREFIX/bitnsbot
CONFIG=$PREFIX/bitnsbot.cfg
DB=$PREFIX/bitnsbot.db
ENVFILE=$PREFIX/deploy.env
UNIT=/etc/systemd/system/$SERVICE.service
CRON=/etc/cron.d/$SERVICE
LOG=/var/log/$SERVICE-update.log

export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

if [ "$(id -u)" -ne 0 ]; then
    echo "error: run as root, e.g. sudo $0" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
BUILD_USER="${BUILD_USER:-${SUDO_USER:-$(stat -c '%U' "$REPO_DIR")}}"
BUILD_HOME="$(getent passwd "$BUILD_USER" | cut -d: -f6)"

if [ ! -f "$REPO_DIR/go.mod" ]; then
    echo "error: $REPO_DIR does not look like the bitnsbot repo (no go.mod)" >&2
    exit 1
fi

echo ">> repo:       $REPO_DIR"
echo ">> build user: $BUILD_USER"
echo ">> prefix:     $PREFIX"

# Build and test as the (non-root) repo owner so Go's caches and the checkout
# stay owned by that user. A non-login shell with an explicit HOME/PATH keeps
# output clean and finds go whether it came from apt (/usr/bin) or the official
# tarball (/usr/local/go/bin).
GOPATHS="/usr/local/go/bin:$BUILD_HOME/go/bin:/usr/local/bin:/usr/bin:/bin"
as_user() {
    runuser -u "$BUILD_USER" -- env HOME="$BUILD_HOME" \
        GOCACHE="$BUILD_HOME/.cache/go-build" PATH="$GOPATHS" \
        bash -c "cd '$REPO_DIR' && $1"
}

if ! as_user 'command -v go >/dev/null'; then
    echo "error: 'go' is not on $BUILD_USER's PATH — install Go first" >&2
    exit 1
fi

echo ">> building"
if ! as_user "go build -o '$REPO_DIR/bitnsbot' ."; then
    echo "error: build failed — nothing installed" >&2
    exit 1
fi
echo ">> testing"
if ! as_user 'go test ./...'; then
    echo "error: tests failed — nothing installed" >&2
    exit 1
fi

echo ">> installing binary to $BINARY"
install -d -o "$BUILD_USER" -m 755 "$PREFIX"
install -o "$BUILD_USER" -m 755 "$REPO_DIR/bitnsbot" "$BINARY"

NEEDS_CONFIG=0
if [ ! -f "$CONFIG" ]; then
    NEEDS_CONFIG=1
    echo ">> writing config template $CONFIG (edit it before the service can run)"
    cat > "$CONFIG" <<'CFG'
# bitnsbot configuration — one flag per line as name=value.
# See CLAUDE.md / main.go for the full flag list. The -config and -db flags in
# the systemd unit take precedence over anything set here.

# Required: Telegram bot token (keep this file private, it is chmod 600).
bot-token=PUT-YOUR-TELEGRAM-BOT-TOKEN-HERE

# Webhook the local telegram-bot-api proxy POSTs updates to.
webhook-url=http://localhost:8080/bot
#api-base-url=http://localhost:8081
#listen=:8080
#secret-token=

# btcd RPC (leave btcd-url empty to run without a node).
btcd-url=wss://localhost:8334/ws
btcd-user=btcd
btcd-pass=btcd
btcd-cert=/home/pi/.btcd/rpc.cert

#verbose=1
CFG
    chown "$BUILD_USER" "$CONFIG"
    chmod 600 "$CONFIG"
fi

echo ">> writing $ENVFILE"
cat > "$ENVFILE" <<ENV
REPO_DIR=$REPO_DIR
BUILD_USER=$BUILD_USER
ENV
chmod 644 "$ENVFILE"

echo ">> installing update script to $PREFIX/update.sh"
install -m 755 "$SCRIPT_DIR/update.sh" "$PREFIX/update.sh"

echo ">> writing systemd unit $UNIT"
cat > "$UNIT" <<UNITEOF
[Unit]
Description=bitnsbot — Bitcoin network events Telegram bot
After=network-online.target btcd.service
Wants=network-online.target

[Service]
Type=simple
User=$BUILD_USER
WorkingDirectory=$PREFIX
ExecStart=$BINARY -config $CONFIG -db $DB
Restart=on-failure
RestartSec=5
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNITEOF

echo ">> writing cron job $CRON (auto-update every 5 minutes)"
cat > "$CRON" <<CRONEOF
# Auto-update bitnsbot from origin/master every 5 minutes.
MAILTO=""
*/5 * * * * root $PREFIX/update.sh >> $LOG 2>&1
CRONEOF
chmod 644 "$CRON"

systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null

if [ "$NEEDS_CONFIG" -eq 1 ]; then
    echo
    echo "!! Service enabled but NOT started — edit $CONFIG (bot-token, btcd, ...), then:"
    echo "!!     sudo systemctl start $SERVICE"
else
    echo ">> starting $SERVICE"
    systemctl restart "$SERVICE"
    sleep 1
    systemctl --no-pager --lines=0 status "$SERVICE" || true
fi

echo
echo ">> done. Follow logs with:"
echo "     journalctl -u $SERVICE -f     # the service"
echo "     tail -f $LOG   # auto-updates"
