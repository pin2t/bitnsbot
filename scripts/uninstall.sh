#!/usr/bin/env bash
# uninstall.sh — stop and remove the bitnsbot systemd service and the auto-update
# cron job. Data under /opt/bitnsbot (binary, config, database) is left in place
# unless --purge is given.
#
#   Run as root:  sudo ./scripts/uninstall.sh [--purge]
set -euo pipefail

PREFIX=/opt/bitnsbot
SERVICE=bitnsbot
UNIT=/etc/systemd/system/$SERVICE.service
CRON=/etc/cron.d/$SERVICE
LOG=/var/log/$SERVICE-update.log

export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

if [ "$(id -u)" -ne 0 ]; then
    echo "error: run as root, e.g. sudo $0" >&2
    exit 1
fi

PURGE=0
if [ "${1:-}" = "--purge" ]; then
    PURGE=1
fi

echo ">> removing cron job $CRON"
rm -f "$CRON"

echo ">> stopping and disabling $SERVICE"
systemctl stop "$SERVICE" 2>/dev/null || true
systemctl disable "$SERVICE" 2>/dev/null || true
rm -f "$UNIT"
systemctl daemon-reload
systemctl reset-failed "$SERVICE" 2>/dev/null || true

if [ "$PURGE" -eq 1 ]; then
    echo ">> purging $PREFIX and $LOG"
    rm -rf "$PREFIX" "$LOG"
else
    echo ">> kept $PREFIX (binary, config, database) — re-run with --purge to remove it"
fi

echo ">> done."
