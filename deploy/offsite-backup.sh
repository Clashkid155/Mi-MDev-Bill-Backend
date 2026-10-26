#!/usr/bin/env bash
# Copies the newest database snapshot off this server, encrypted, so that losing
# the server (or the provider closing the account) does not lose the business.
#
# The server saves a snapshot to BACKUP_DIR every hour (see BACKUP_EVERY_MINUTES).
# This script runs from cron an hour later-ish, compresses the newest one,
# encrypts it with your PUBLIC age key (only your private key, kept off the
# server, can open it) and uploads it with rclone. It pings a free
# healthchecks.io check on success; if the ping stops, healthchecks.io emails you.
#
# Settings come from /etc/bills/offsite.env (see deploy/README.md):
#   AGE_RECIPIENT   the public key printed by `age-keygen` (starts with age1...)
#   RCLONE_REMOTE   e.g. r2:bills-backups   (a bucket in Cloudflare R2)
#   HEALTHCHECK_URL optional, the ping URL from healthchecks.io
set -euo pipefail

DIR=${BACKUP_DIR:-/var/backups/bills}
REMOTE=${RCLONE_REMOTE:?set RCLONE_REMOTE, like r2:bills-backups}
RECIPIENT=${AGE_RECIPIENT:?set AGE_RECIPIENT to the public key from age-keygen}
PING=${HEALTHCHECK_URL:-}

fail() {
	echo "offsite backup failed: $*" >&2
	[ -n "$PING" ] && curl -fsS -m 10 --retry 2 "$PING/fail" >/dev/null || true
	exit 1
}

latest=$(ls -1 "$DIR"/bills-*.db 2>/dev/null | sort | tail -n 1 || true)
[ -n "$latest" ] || fail "no snapshot in $DIR"
# A snapshot older than 3 hours means the server stopped making them.
[ -n "$(find "$latest" -mmin -180)" ] || fail "the newest snapshot ($latest) is more than 3 hours old"

name=$(basename "$latest")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

gzip -c "$latest" | age -r "$RECIPIENT" >"$work/$name.gz.age"
rclone copyto "$work/$name.gz.age" "$REMOTE/$name.gz.age" || fail "upload"

# One copy a day is kept for 90 days, the hourly ones for 14.
if [ "$(date -u +%H)" = "03" ]; then
	rclone copyto "$work/$name.gz.age" "$REMOTE/daily/$name.gz.age" || fail "daily copy"
fi
rclone delete "$REMOTE" --min-age 14d --exclude "daily/**" || fail "clean up hourly copies"
rclone delete "$REMOTE/daily" --min-age 90d 2>/dev/null || true

[ -n "$PING" ] && curl -fsS -m 10 --retry 3 "$PING" >/dev/null || true
echo "offsite backup ok: $name"
