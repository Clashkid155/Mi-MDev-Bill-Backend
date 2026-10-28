#!/usr/bin/env bash
# Puts the current code on the server in one command, safely:
#
#   deploy/deploy.sh [ssh-host]        # default host: bills (the name in ~/.ssh/config)
#
# 1. checks the code (vet, all tests with the race detector, known vulnerabilities),
# 2. builds the program with the Go version pinned in go.mod,
# 3. copies it to the server and checks the copy is byte for byte the same,
# 4. keeps the running version as bills-backend.prev, swaps the new one in and restarts
#    (the restart waits for purchases in progress to finish),
# 5. checks the server answers; if it does not, puts the previous version back.
#
# Options (environment variables):
#   SKIP_CHECKS=1   skip step 1 (only when you must ship a fix in a hurry)
#   DRY_RUN=1       do steps 1 and 2 and stop, touching nothing on the server
#   TARGET_ARCH=arm64   for an ARM server
#
# REMOTE_APP_DIR, REMOTE_TMP and HEALTH_URL name where things are on the server; the defaults
# are right for the setup in deploy/README.md. They exist so deploy/tests/run.sh can run this
# script against a fake server.
set -euo pipefail

HOST=${1:-bills}
ARCH=${TARGET_ARCH:-amd64}
APP_DIR=${REMOTE_APP_DIR:-/opt/bills}
TMP_DIR=${REMOTE_TMP:-/tmp}
HEALTH_URL=${HEALTH_URL:-http://127.0.0.1:8080/healthz}

here=$(cd "$(dirname "$0")/.." && pwd)
cd "$here" || exit 1
PATH="$PATH:$(go env GOPATH)/bin"
export PATH

version=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
if ! git diff --quiet HEAD -- 2>/dev/null; then
	version="$version-uncommitted"
	echo "note: there are uncommitted changes; deploying them as $version"
fi

if [ -z "${SKIP_CHECKS:-}" ]; then
	echo "== 1. checking the code"
	test -z "$(gofmt -l .)" || { echo "gofmt would change: $(gofmt -l .)" >&2; exit 1; }
	go vet ./...
	go test -race -count=1 ./... 2>&1 | grep -v "no test files"
	if command -v govulncheck >/dev/null; then govulncheck ./... 2>&1 | tail -1; else echo "(govulncheck not installed, skipping the vulnerability scan)"; fi
fi

echo "== 2. building $version for linux/$ARCH"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -ldflags="-s -w -X main.version=$version" -o "$out/bills-backend" .
sum=$(shasum -a 256 "$out/bills-backend" | cut -d' ' -f1)
echo "   built $(du -h "$out/bills-backend" | cut -f1), sha256 ${sum:0:16}..."
[ -z "${DRY_RUN:-}" ] || { echo "dry run: stopping before the server"; exit 0; }

echo "== 3. copying to $HOST"
scp -q "$out/bills-backend" "$HOST:$TMP_DIR/bills-backend.new"
# $sum and $TMP_DIR are meant to be filled in here, on this machine, before the command is sent.
# shellcheck disable=SC2029
ssh "$HOST" "echo '$sum  $TMP_DIR/bills-backend.new' | sha256sum -c - >/dev/null" || {
	echo "the copy on the server differs from what was built; nothing was changed" >&2
	exit 1
}
echo "   the copy matches"

echo "== 4. swapping it in and restarting"
# shellcheck disable=SC2029
ssh "$HOST" "sudo bash -s -- '$APP_DIR' '$TMP_DIR'" <<'REMOTE_SCRIPT'
set -e
app=$1
tmp=$2
cp -p "$app/bills-backend" "$app/bills-backend.prev"
install -m 755 -o root -g root "$tmp/bills-backend.new" "$app/bills-backend"
rm -f "$tmp/bills-backend.new"
systemctl restart bills
REMOTE_SCRIPT

echo "== 5. checking the server answers"
ok=
for _ in $(seq 1 30); do
	# shellcheck disable=SC2029
	if ssh "$HOST" "curl -fsS -m 3 '$HEALTH_URL' >/dev/null 2>&1"; then
		ok=1
		break
	fi
	sleep "${HEALTH_WAIT:-1}"
done
if [ -z "$ok" ]; then
	echo "the new version did not answer within 30 seconds: putting the previous version back" >&2
	# shellcheck disable=SC2029
	ssh "$HOST" "sudo bash -s -- '$APP_DIR'" <<'REMOTE_SCRIPT' || true
set -e
app=$1
install -m 755 -o root -g root "$app/bills-backend.prev" "$app/bills-backend"
systemctl restart bills
journalctl -u bills -n 15 --no-pager -o cat
REMOTE_SCRIPT
	exit 1
fi
ssh "$HOST" "journalctl -u bills -n 6 --no-pager -o cat | cut -c1-200"
echo "deployed $version to $HOST. The previous version is kept as $APP_DIR/bills-backend.prev."
