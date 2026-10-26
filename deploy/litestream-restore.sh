#!/usr/bin/env bash
# Restores the database from the Litestream bucket and checks the result with the
# app's own checker. Use it to test the continuous backup (run it monthly with
# the restore-check.sh test of the encrypted snapshots), or to recover for real.
#
#   export CF_ACCOUNT_ID=... R2_ACCESS_KEY=... R2_SECRET_KEY=...   # a token that can read the bucket
#   deploy/litestream-restore.sh                       # restores to a temporary file and checks it
#   deploy/litestream-restore.sh /var/lib/bills/bills.db   # recovery: write the real database
#
# Needs the litestream program and the app binary (./bills-backend) on this machine.
set -euo pipefail

CONFIG=${LITESTREAM_CONFIG:-$(dirname "$0")/litestream.yml}
DB_IN_CONFIG=${DB_PATH:-/var/lib/bills/bills.db} # the path the config names, which picks the replica
BIN=${BIN:-./bills-backend}
OUT=${1:-}
if [ -z "$OUT" ]; then
	OUT=$(mktemp -d)/restored.db
fi

litestream restore -config "$CONFIG" -integrity-check full -o "$OUT" "$DB_IN_CONFIG"
"$BIN" verify-backup "$OUT"
echo "restored to $OUT"
