#!/usr/bin/env bash
# Puts the tools built by deploy/build-tools.sh onto the server and restarts what uses them.
#
#   deploy/install-tools.sh [ssh-host]        # default host: bills
#
# Installs caddy, litestream, rclone, age and age-keygen into /usr/local/bin (they take
# priority over the copies from the package manager, which are removed for age and rclone),
# points Caddy's service at the new binary, and checks each service comes back healthy. The
# previous binaries are kept in /opt/bills/tools.prev, and if Caddy does not come back the
# service goes back to the packaged binary at once.
#
# REMOTE_BIN_DIR, REMOTE_APP_DIR, REMOTE_UNIT_DIR, REMOTE_CADDYFILE and REMOTE_TMP name where
# things are on the server; the defaults are right for deploy/README.md. They exist so
# deploy/tests/run.sh can run this script against a fake server.
set -euo pipefail

HOST=${1:-bills}
BIN_DIR=${REMOTE_BIN_DIR:-/usr/local/bin}
APP_DIR=${REMOTE_APP_DIR:-/opt/bills}
UNIT_DIR=${REMOTE_UNIT_DIR:-/etc/systemd/system}
CADDYFILE=${REMOTE_CADDYFILE:-/etc/caddy/Caddyfile}
TMP_DIR=${REMOTE_TMP:-/tmp}

here=$(cd "$(dirname "$0")/.." && pwd)
DIR=${TOOLS_DIR:-$here/build/tools}
cd "$DIR" || { echo "no built tools in $DIR: run deploy/build-tools.sh first" >&2; exit 1; }
echo "== checking the build is intact"
shasum -a 256 -c SHA256SUMS
tools=(caddy litestream rclone age age-keygen)

echo "== copying to $HOST"
# shellcheck disable=SC2029
ssh "$HOST" "mkdir -p '$TMP_DIR/tools' && rm -f '$TMP_DIR'/tools/*"
scp -q "${tools[@]}" SHA256SUMS "$HOST:$TMP_DIR/tools/"
# shellcheck disable=SC2029
ssh "$HOST" "cd '$TMP_DIR/tools' && sha256sum -c SHA256SUMS >/dev/null" || {
	echo "the copies on the server differ from what was built; nothing was installed" >&2
	exit 1
}
echo "   the copies match"

echo "== installing"
# shellcheck disable=SC2029
ssh "$HOST" "sudo bash -s -- '$BIN_DIR' '$APP_DIR' '$UNIT_DIR' '$CADDYFILE' '$TMP_DIR'" <<'REMOTE_SCRIPT'
set -euo pipefail
bin=$1
app=$2
units=$3
caddyfile=$4
tmp=$5
tools="caddy litestream rclone age age-keygen"

mkdir -p "$app/tools.prev"
for t in $tools; do
	[ -f "$bin/$t" ] && cp -p "$bin/$t" "$app/tools.prev/$t" || true
	install -m 755 -o root -g root "$tmp/tools/$t" "$bin/$t.new"
	mv -f "$bin/$t.new" "$bin/$t"
done
rm -rf "$tmp/tools"

# Caddy: use the new binary, after checking it accepts the live config (as the caddy user, so
# it cannot leave root owned log files behind).
if ! runuser -u caddy -- "$bin/caddy" validate --config "$caddyfile" --adapter caddyfile >/dev/null 2>&1; then
	echo "the new Caddy does not accept the config; leaving the packaged Caddy in use" >&2
	exit 1
fi
domain=$(awk '/^[A-Za-z0-9.-]+ *\{/ {print $1; exit}' "$caddyfile")
mkdir -p "$units/caddy.service.d"
printf '%s\n' \
	'# Written by deploy/install-tools.sh: run the Caddy built with the newest Go and libraries.' \
	'[Service]' \
	'ExecStart=' \
	"ExecStart=$bin/caddy run --environ --config $caddyfile" \
	'ExecReload=' \
	"ExecReload=$bin/caddy reload --config $caddyfile --force" \
	>"$units/caddy.service.d/10-patched-binary.conf"
systemctl daemon-reload
systemctl restart caddy
sleep "${SETTLE_WAIT:-3}"
answer=$(curl -sk -m 10 --resolve "$domain:443:127.0.0.1" -o /dev/null -w '%{http_code}' "https://$domain/healthz" || true)
if ! systemctl is-active --quiet caddy || ! echo "$answer" | grep -qE '^(200|502)$'; then
	echo "Caddy did not come back (answer: $answer): restoring the packaged Caddy" >&2
	rm -f "$units/caddy.service.d/10-patched-binary.conf"
	systemctl daemon-reload
	systemctl restart caddy
	exit 1
fi

# Litestream: restart so it runs the new binary, then make sure it is replicating without errors.
# (On a first install it is not enabled yet, so there is nothing to restart.)
if systemctl is-enabled --quiet litestream 2>/dev/null || systemctl is-active --quiet litestream 2>/dev/null; then
	systemctl restart litestream
	sleep "${LITESTREAM_WAIT:-6}"
	if ! systemctl is-active --quiet litestream || journalctl -u litestream --since "-10s" -o cat --no-pager | grep -q "level=ERROR"; then
		echo "Litestream is not healthy after the restart; the old binary is in $app/tools.prev" >&2
		exit 1
	fi
fi

# The package manager's age and rclone are shadowed by the newer ones; remove them so there is one of each.
DEBIAN_FRONTEND=noninteractive apt-get -y -qq remove age rclone >/dev/null 2>&1 || true
echo "installed:"
echo "  caddy      $("$bin/caddy" version | cut -d' ' -f1)"
echo "  litestream $("$bin/litestream" version)"
echo "  rclone     $("$bin/rclone" version | head -1)"
echo "  age        $("$bin/age" --version)"
REMOTE_SCRIPT
echo "done. The previous versions are in $APP_DIR/tools.prev on the server."
