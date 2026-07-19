#!/usr/bin/env bash
# update.sh — if origin/master has moved, check it out, rebuild + test, and on
# success atomically replace /opt/bitnsbot/bitnsbot and restart the service.
# Installed to /opt/bitnsbot/update.sh and run every 5 minutes by cron; also
# safe to run by hand as root. Stays silent when there is nothing to do, so the
# cron log only grows on real updates.
set -euo pipefail

PREFIX=/opt/bitnsbot
SERVICE=bitnsbot
BINARY=$PREFIX/bitnsbot
ENVFILE=$PREFIX/deploy.env

export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

ts() { date '+%Y-%m-%d %H:%M:%S'; }

if [ "$(id -u)" -ne 0 ]; then
    echo "$(ts) error: run as root" >&2
    exit 1
fi

# One run at a time — a build+test can outlast the 5-minute cron interval.
exec 9>"/run/$SERVICE-update.lock"
if ! flock -n 9; then
    exit 0
fi

if [ ! -f "$ENVFILE" ]; then
    echo "$(ts) error: $ENVFILE missing — run install.sh first" >&2
    exit 1
fi
# shellcheck source=/dev/null
source "$ENVFILE"
: "${REPO_DIR:?deploy.env missing REPO_DIR}" "${BUILD_USER:?deploy.env missing BUILD_USER}"
BUILD_HOME="$(getent passwd "$BUILD_USER" | cut -d: -f6)"

# git/go run as the repo owner (avoids git "dubious ownership" and keeps caches
# owned by that user). A non-login shell + explicit HOME/PATH keeps captured git
# output clean and finds go from either apt or the official tarball.
GOPATHS="/usr/local/go/bin:$BUILD_HOME/go/bin:/usr/local/bin:/usr/bin:/bin"
as_user() {
    runuser -u "$BUILD_USER" -- env HOME="$BUILD_HOME" \
        GOCACHE="$BUILD_HOME/.cache/go-build" PATH="$GOPATHS" \
        bash -c "cd '$REPO_DIR' && $1"
}

if ! as_user 'git fetch --quiet origin'; then
    echo "$(ts) git fetch failed; will retry next run"
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
    echo "$(ts) local checkout is ahead of / diverged from origin/master — skipping"
    exit 0
fi

echo "$(ts) update available: ${LOCAL:0:12} -> ${REMOTE:0:12}"
if ! as_user 'git checkout --quiet master && git merge --ff-only --quiet origin/master'; then
    echo "$(ts) fast-forward failed unexpectedly — aborting"
    exit 1
fi

echo "$(ts) building"
if ! as_user "go build -o '$REPO_DIR/bitnsbot' ."; then
    echo "$(ts) BUILD FAILED — keeping the running binary"
    exit 1
fi
echo "$(ts) testing"
if ! as_user 'go test ./...'; then
    echo "$(ts) TESTS FAILED — keeping the running binary"
    exit 1
fi

echo "$(ts) deploying and restarting $SERVICE"
install -o "$BUILD_USER" -m 755 "$REPO_DIR/bitnsbot" "$BINARY.new"
mv -f "$BINARY.new" "$BINARY"
systemctl restart "$SERVICE"
echo "$(ts) updated to $(as_user 'git rev-parse --short HEAD') and restarted"
