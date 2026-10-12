#!/usr/bin/env bash
# update.sh — if origin/master has moved, check it out, rebuild + test, and on
# success atomically replace /opt/bitnsbot/bitnsbot and restart the service.
# Installed to /opt/bitnsbot/update.sh and run every 5 minutes by
# bitnsbot-update.timer, as bitnsbot-update.service, so what it prints goes to
# the journal (journalctl -u bitnsbot-update); also safe to run by hand as root.
# Stays silent when there is nothing to do, so the journal only hears of real
# updates.
set -euo pipefail

PREFIX=/opt/bitnsbot
SERVICE=bitnsbot
BINARY=$PREFIX/bitnsbot
ENVFILE=$PREFIX/deploy.env

export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# Under the journal (stdout is the stream JOURNAL_STREAM names) a line carries
# its syslog priority instead of a timestamp, which the journal adds itself:
# say is info, warn <4>, fail <3>, so journalctl -p err finds a failed update.
# Run by hand, each line is timestamped instead.
JOURNAL=0
if [ -n "${JOURNAL_STREAM:-}" ] && [ "$(stat -L -c '%d:%i' "/proc/$$/fd/1" 2>/dev/null)" = "$JOURNAL_STREAM" ]; then
    JOURNAL=1
fi
line() {
    if [ "$JOURNAL" -eq 1 ]; then echo "$1$2"; else echo "$(date '+%Y-%m-%d %H:%M:%S') $2"; fi
}
say()  { line "" "$*"; }
warn() { line "<4>" "$*"; }
fail() { line "<3>" "$*"; }

if [ "$(id -u)" -ne 0 ]; then
    fail "error: run as root" >&2
    exit 1
fi

# One run at a time — a timer starts no second run of a service still running,
# but a run by hand can overlap one.
exec 9>"/run/$SERVICE-update.lock"
if ! flock -n 9; then
    exit 0
fi

if [ ! -f "$ENVFILE" ]; then
    fail "error: $ENVFILE missing — run install.sh first" >&2
    exit 1
fi
# shellcheck source=/dev/null
source "$ENVFILE"
: "${REPO_DIR:?deploy.env missing REPO_DIR}" "${BUILD_USER:?deploy.env missing BUILD_USER}"
BUILD_HOME="$(getent passwd "$BUILD_USER" | cut -d: -f6)"

# git/go run as the repo owner (avoids git "dubious ownership" and keeps caches
# owned by that user). setpriv rather than runuser, which opens a PAM session
# for every call and so logs two lines to this unit's journal each time —
# several every five minutes, with nothing to update. A non-login shell + explicit HOME/PATH keeps captured git
# output clean and finds go from either apt or the official tarball.
GOPATHS="/usr/local/go/bin:$BUILD_HOME/go/bin:/usr/local/bin:/usr/bin:/bin"
as_user() {
    setpriv --reuid="$BUILD_USER" --regid="$(id -g "$BUILD_USER")" --init-groups -- env HOME="$BUILD_HOME" \
        GOCACHE="$BUILD_HOME/.cache/go-build" PATH="$GOPATHS" \
        bash -c "cd '$REPO_DIR' && $1"
}

if ! as_user 'git fetch --quiet origin'; then
    warn "git fetch failed; will retry next run"
    exit 0
fi
LOCAL="$(as_user 'git rev-parse HEAD')"
REMOTE="$(as_user 'git rev-parse origin/master')"
if [ "$LOCAL" = "$REMOTE" ]; then
    exit 0
fi
# Act only when origin/master is strictly ahead of what's deployed (a clean
# fast-forward). If the local checkout is ahead of or has diverged from
# origin/master there is nothing to pull — skip, rather than rebuild every run.
if ! as_user 'git merge-base --is-ancestor HEAD origin/master'; then
    warn "local checkout is ahead of / diverged from origin/master — skipping"
    exit 0
fi

say "update available: ${LOCAL:0:12} -> ${REMOTE:0:12}"
if ! as_user 'git checkout --quiet master && git merge --ff-only --quiet origin/master'; then
    fail "fast-forward failed unexpectedly — aborting"
    exit 1
fi

say "building"
if ! as_user "go build -o '$REPO_DIR/bitnsbot' ."; then
    fail "BUILD FAILED — keeping the running binary"
    exit 1
fi
say "testing"
if ! as_user 'go test ./...'; then
    fail "TESTS FAILED — keeping the running binary"
    exit 1
fi

say "deploying and restarting $SERVICE"
install -o "$BUILD_USER" -m 755 "$REPO_DIR/bitnsbot" "$BINARY.new"
mv -f "$BINARY.new" "$BINARY"
systemctl restart "$SERVICE"
say "updated to $(as_user 'git rev-parse --short HEAD') and restarted"
