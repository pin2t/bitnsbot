#!/usr/bin/env bash
# static.sh — serve the landing page in this checkout's static/ with lighttpd,
# as its own systemd service (bitnsbot-static) bound to 127.0.0.1:8086. Called
# by install.sh; safe to run on its own and to re-run.
#
#   Run from the repo:  sudo ./scripts/static.sh
#
# lighttpd serves the files straight out of the checkout, so every update.sh
# fast-forward puts the new page live with no restart. It runs as the
# checkout's owner rather than www-data, because a home directory is often
# mode 700 (Raspberry Pi OS's is) and www-data could not read it, and from a
# config of its own, so the stock lighttpd.conf and lighttpd.service are never
# used. When this script installs lighttpd it masks the stock service first:
# Debian's package starts it on port 80 the moment it is installed. A lighttpd
# that was already installed keeps its stock service exactly as it was.
#
# Defaults can be overridden with env vars, e.g.
#   sudo STATIC_PORT=8090 ./scripts/static.sh
set -euo pipefail

STATIC_SERVICE="${STATIC_SERVICE:-bitnsbot-static}"
STATIC_BIND="${STATIC_BIND:-127.0.0.1}"
STATIC_PORT="${STATIC_PORT:-8086}"
STATIC_CONF=/etc/lighttpd/$STATIC_SERVICE.conf
STATIC_UNIT=/etc/systemd/system/$STATIC_SERVICE.service
LIGHTTPD=/usr/sbin/lighttpd

export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

if [ "$(id -u)" -ne 0 ]; then
    echo "error: run as root, e.g. sudo $0" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
STATIC_DIR="$REPO_DIR/static"
BUILD_USER="${BUILD_USER:-${SUDO_USER:-$(stat -c '%U' "$REPO_DIR")}}"

if [ ! -f "$STATIC_DIR/index.html" ]; then
    echo "error: $STATIC_DIR/index.html not found — nothing to serve" >&2
    exit 1
fi

echo ">> static page: $STATIC_DIR on http://$STATIC_BIND:$STATIC_PORT (as $BUILD_USER)"

if [ ! -x "$LIGHTTPD" ]; then
    if ! command -v apt-get >/dev/null; then
        echo "error: lighttpd is not installed and there is no apt-get to install it" >&2
        exit 1
    fi
    echo ">> installing lighttpd (its stock service is masked first, so nothing starts on port 80)"
    systemctl mask lighttpd.service >/dev/null
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq lighttpd >/dev/null
fi

echo ">> writing $STATIC_CONF"
install -d -m 755 "$(dirname "$STATIC_CONF")"
cat > "$STATIC_CONF" <<CONFEOF
# The bitnsbot landing page: $STATIC_DIR on $STATIC_BIND:$STATIC_PORT, run as
# $BUILD_USER by $STATIC_SERVICE.service. Written by scripts/static.sh; standalone,
# so lighttpd.conf and the stock service are not used. Directory listings are
# off, mod_dirlisting not being loaded.
server.document-root = "$STATIC_DIR"
server.bind          = "$STATIC_BIND"
server.port          = $STATIC_PORT
server.tag           = ""
index-file.names     = ( "index.html" )
mimetype.assign      = (
    ".html" => "text/html; charset=utf-8",
    ".css"  => "text/css; charset=utf-8",
    ".js"   => "text/javascript; charset=utf-8",
    ".svg"  => "image/svg+xml",
    ".jpeg" => "image/jpeg",
    ".jpg"  => "image/jpeg",
    ".png"  => "image/png",
    ".webp" => "image/webp",
    ".ico"  => "image/x-icon",
    ".txt"  => "text/plain; charset=utf-8",
)
CONFEOF
chmod 644 "$STATIC_CONF"

if ! "$LIGHTTPD" -tt -f "$STATIC_CONF"; then
    echo "error: lighttpd rejected $STATIC_CONF" >&2
    exit 1
fi

echo ">> writing systemd unit $STATIC_UNIT"
cat > "$STATIC_UNIT" <<UNITEOF
[Unit]
Description=bitnsbot landing page (lighttpd serving $STATIC_DIR on $STATIC_BIND:$STATIC_PORT)
After=network.target

[Service]
User=$BUILD_USER
Group=$(id -gn "$BUILD_USER")
ExecStartPre=$LIGHTTPD -tt -f $STATIC_CONF
ExecStart=$LIGHTTPD -D -f $STATIC_CONF
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
UNITEOF

systemctl daemon-reload
systemctl enable "$STATIC_SERVICE" >/dev/null
echo ">> starting $STATIC_SERVICE"
systemctl restart "$STATIC_SERVICE"
sleep 1
if ! systemctl is-active --quiet "$STATIC_SERVICE"; then
    echo "error: $STATIC_SERVICE did not start — see journalctl -u $STATIC_SERVICE" >&2
    exit 1
fi
if command -v curl >/dev/null; then
    CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://$STATIC_BIND:$STATIC_PORT/" || true)"
    echo ">> GET http://$STATIC_BIND:$STATIC_PORT/ -> $CODE"
fi
