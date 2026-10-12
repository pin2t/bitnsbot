#!/usr/bin/env bash
# uninstall.sh — stop and remove the bitnsbot systemd service and the auto-update
# timer. Data under /opt/bitnsbot (binary, config, database) is left in place
# unless --purge is given. The bitnsbot-static landing page service and its
# lighttpd config go too; the lighttpd package itself is left installed.
#
#   Run as root:  sudo ./scripts/uninstall.sh [--purge]
set -euo pipefail

PREFIX=/opt/bitnsbot
SERVICE=bitnsbot
UNIT=/etc/systemd/system/$SERVICE.service
UPDATE_SERVICE=$SERVICE-update
UPDATE_UNIT=/etc/systemd/system/$UPDATE_SERVICE.service
UPDATE_TIMER=/etc/systemd/system/$UPDATE_SERVICE.timer
CRON=/etc/cron.d/$SERVICE
LOG=/var/log/$SERVICE-update.log
STATIC_SERVICE="${STATIC_SERVICE:-bitnsbot-static}"
STATIC_UNIT=/etc/systemd/system/$STATIC_SERVICE.service
STATIC_CONF=/etc/lighttpd/$STATIC_SERVICE.conf

export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

if [ "$(id -u)" -ne 0 ]; then
    echo "error: run as root, e.g. sudo $0" >&2
    exit 1
fi

PURGE=0
if [ "${1:-}" = "--purge" ]; then
    PURGE=1
fi

echo ">> stopping and removing the auto-update timer $UPDATE_SERVICE"
systemctl disable --now "$UPDATE_SERVICE.timer" 2>/dev/null || true
systemctl stop "$UPDATE_SERVICE.service" 2>/dev/null || true
rm -f "$UPDATE_TIMER" "$UPDATE_UNIT" "$CRON"

echo ">> stopping and disabling $SERVICE"
systemctl stop "$SERVICE" 2>/dev/null || true
systemctl disable "$SERVICE" 2>/dev/null || true
rm -f "$UNIT"
systemctl daemon-reload
systemctl reset-failed "$SERVICE" "$UPDATE_SERVICE" 2>/dev/null || true

echo ">> stopping and removing $STATIC_SERVICE (the landing page)"
systemctl stop "$STATIC_SERVICE" 2>/dev/null || true
systemctl disable "$STATIC_SERVICE" 2>/dev/null || true
rm -f "$STATIC_UNIT" "$STATIC_CONF"
systemctl daemon-reload
systemctl reset-failed "$STATIC_SERVICE" 2>/dev/null || true

if [ "$PURGE" -eq 1 ]; then
    echo ">> purging $PREFIX and $LOG"
    rm -rf "$PREFIX" "$LOG"
else
    echo ">> kept $PREFIX (binary, config, database) — re-run with --purge to remove it"
fi

echo ">> done."
