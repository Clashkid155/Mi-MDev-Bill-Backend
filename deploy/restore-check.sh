#!/usr/bin/env bash
# Proves the backups really work: downloads the newest copy from the bucket,
# decrypts it with YOUR private key and opens it with the app's own checker.
#
# Run this on your own computer (not the server) once a month:
#   brew install age rclone        # once, plus an rclone remote named like RCLONE_REMOTE
#   go build -o bills-backend .    # the same code that is deployed
#   AGE_KEY=~/keys/bills-age.txt RCLONE_REMOTE=r2:bills-backups deploy/restore-check.sh
#
# It prints how many orders the backup holds and when the latest one was made.
# Compare that with what you expect; if the numbers look wrong or it says
# BAD BACKUP, fix the backups the same day.
set -euo pipefail

REMOTE=${RCLONE_REMOTE:?set RCLONE_REMOTE, like r2:bills-backups}
KEY=${AGE_KEY:?set AGE_KEY to the file holding your private age key}
BIN=${BIN:-./bills-backend}

latest=$(rclone lsf "$REMOTE" --files-only | grep '\.gz\.age$' | sort | tail -n 1)
[ -n "$latest" ] || { echo "no backups found in $REMOTE" >&2; exit 1; }
echo "newest backup: $latest"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
rclone copyto "$REMOTE/$latest" "$work/backup.gz.age"
age -d -i "$KEY" "$work/backup.gz.age" | gunzip >"$work/restored.db"
"$BIN" verify-backup "$work/restored.db"
