#!/usr/bin/env bash
# Tests deploy/deploy.sh and deploy/install-tools.sh against a FAKE server, so the failure
# paths (a corrupted copy, a server that does not come back, Caddy rejecting its config,
# Litestream logging errors) can be run without touching the real one.
#
#   deploy/tests/run.sh
#
# The "server" is a folder: ssh and scp are replaced by small scripts that run the remote
# commands locally against it, and systemctl, curl, journalctl, runuser, apt-get and install
# are stand-ins that record what they were asked and fail on request. It proves the control
# flow of the scripts (what is installed, kept, rolled back, and what the exit code is). It
# does not prove that real systemd, Caddy or Litestream behave; the live runs did that.
set -uo pipefail

here=$(cd "$(dirname "$0")/../.." && pwd)
pass=0
fail=0
ROOT=

ok() { pass=$((pass + 1)); printf '  ok    %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL  %s\n' "$1"; [ -z "${2:-}" ] || printf '        %s\n' "$2"; }
check() { if eval "$2"; then ok "$1"; else bad "$1" "expected: $2"; fi; }

# ---- the fake server ----
new_server() {
	ROOT=$(mktemp -d)
	mkdir -p "$ROOT"/{opt/bills,tmp,bin,units,etc,shims,tools}
	echo OLD >"$ROOT/opt/bills/bills-backend"
	for t in caddy litestream rclone age age-keygen; do echo "OLD $t" >"$ROOT/bin/$t"; done
	printf 'test.example.com {\n\treverse_proxy 127.0.0.1:8080\n}\n' >"$ROOT/etc/Caddyfile"
	: >"$ROOT/calls.log"

	cat >"$ROOT/shims/ssh" <<'SHIM'
#!/usr/bin/env bash
shift                       # the host
cmd="$*"
cmd=${cmd#sudo }            # the fake server is already "root"
exec bash -c "$cmd"
SHIM
	cat >"$ROOT/shims/scp" <<'SHIM'
#!/usr/bin/env bash
args=()
for a in "$@"; do [ "$a" = "-q" ] || args+=("$a"); done
dest=${args[${#args[@]}-1]#*:}
unset 'args[${#args[@]}-1]'
for f in "${args[@]}"; do
	if [ -d "$dest" ] || [[ "$dest" == */ ]]; then mkdir -p "$dest"; target="$dest/$(basename "$f")"; else target=$dest; fi
	cp "$f" "$target"
	if [ -n "${TEST_CORRUPT:-}" ] && [ "$(basename "$f")" = "$TEST_CORRUPT" ]; then printf 'x' >>"$target"; fi
done
SHIM
	cat >"$ROOT/shims/systemctl" <<'SHIM'
#!/usr/bin/env bash
echo "systemctl $*" >>"$FAKE_ROOT/calls.log"
case "$1" in
	is-active | is-enabled)
		unit=${*: -1}
		[ "$unit" = caddy ] && [ -n "${TEST_CADDY_DOWN:-}" ] && exit 3
		[ "$unit" = litestream ] && [ -n "${TEST_LITESTREAM_OFF:-}" ] && exit 3
		exit 0 ;;
esac
exit 0
SHIM
	cat >"$ROOT/shims/curl" <<'SHIM'
#!/usr/bin/env bash
echo "curl $*" >>"$FAKE_ROOT/calls.log"
if [[ "$*" == *"-w"* ]]; then printf '%s' "${TEST_CADDY_ANSWER:-502}"; exit 0; fi   # install-tools: Caddy's answer
[ -z "${TEST_HEALTH_FAIL:-}" ]                                                       # deploy: the program's health check
SHIM
	cat >"$ROOT/shims/journalctl" <<'SHIM'
#!/usr/bin/env bash
if [[ "$*" == *litestream* ]] && [ -n "${TEST_LITESTREAM_ERR:-}" ]; then echo 'time=now level=ERROR msg="boom"'; else echo 'level=INFO fine'; fi
SHIM
	cat >"$ROOT/shims/runuser" <<'SHIM'
#!/usr/bin/env bash
echo "runuser $*" >>"$FAKE_ROOT/calls.log"
[ -z "${TEST_CADDY_VALIDATE_FAIL:-}" ]
SHIM
	cat >"$ROOT/shims/apt-get" <<'SHIM'
#!/usr/bin/env bash
echo "apt-get $*" >>"$FAKE_ROOT/calls.log"
SHIM
	cat >"$ROOT/shims/install" <<'SHIM'
#!/usr/bin/env bash
args=()
while [ $# -gt 0 ]; do case "$1" in -o | -g) shift 2 ;; *) args+=("$1"); shift ;; esac; done
exec /usr/bin/install "${args[@]}"
SHIM
	cat >"$ROOT/shims/sha256sum" <<'SHIM'
#!/usr/bin/env bash
exec shasum -a 256 "$@"
SHIM
	chmod +x "$ROOT"/shims/*
}

# fake_tools writes the "built tools" folder: scripts that print a version, plus the checksum list.
fake_tools() {
	for t in caddy litestream rclone age age-keygen; do
		printf '#!/bin/sh\necho "NEW %s v1"\n' "$t" >"$ROOT/tools/$t"
		chmod +x "$ROOT/tools/$t"
	done
	(cd "$ROOT/tools" && shasum -a 256 age age-keygen caddy litestream rclone >SHA256SUMS)
}

# in_server runs a script with the fake server's environment.
in_server() {
	env FAKE_ROOT="$ROOT" PATH="$ROOT/shims:$PATH" \
		REMOTE_APP_DIR="$ROOT/opt/bills" REMOTE_TMP="$ROOT/tmp" REMOTE_BIN_DIR="$ROOT/bin" \
		REMOTE_UNIT_DIR="$ROOT/units" REMOTE_CADDYFILE="$ROOT/etc/Caddyfile" TOOLS_DIR="$ROOT/tools" \
		HEALTH_WAIT=0 SETTLE_WAIT=0 LITESTREAM_WAIT=0 SKIP_CHECKS=1 "$@"
}
restarts() { grep -c "^systemctl restart $1\$" "$ROOT/calls.log"; }

# ---- deploy.sh ----
echo "== deploy.sh"
cd "$here" || exit 1

new_server
out=$(in_server "$here/deploy/deploy.sh" fake 2>&1); code=$?
check "happy path: exits 0" "[ $code = 0 ]"
check "happy path: the new program is in place" "[ \"\$(head -c 3 '$ROOT/opt/bills/bills-backend')\" != OLD ] && [ \$(wc -c <'$ROOT/opt/bills/bills-backend') -gt 1000000 ]"
check "happy path: the old one is kept as .prev" "[ \"\$(cat '$ROOT/opt/bills/bills-backend.prev')\" = OLD ]"
check "happy path: the service is restarted exactly once" "[ \$(restarts bills) = 1 ]"
check "happy path: the installed file is the one that was built" "echo \"$out\" | grep -o 'sha256 [0-9a-f]*' | cut -d' ' -f2 | grep -q \"^\$(shasum -a 256 '$ROOT/opt/bills/bills-backend' | cut -c1-16)\""
check "happy path: nothing is left in the temp folder" "[ ! -e '$ROOT/tmp/bills-backend.new' ]"

new_server
out=$(TEST_CORRUPT=bills-backend in_server "$here/deploy/deploy.sh" fake 2>&1); code=$?
check "corrupted copy: exits non-zero" "[ $code != 0 ]"
check "corrupted copy: says so" "echo \"$out\" | grep -q 'differs from what was built'"
check "corrupted copy: the running program is untouched" "[ \"\$(cat '$ROOT/opt/bills/bills-backend')\" = OLD ]"
check "corrupted copy: the service is NOT restarted" "[ \$(restarts bills) = 0 ]"

new_server
out=$(TEST_HEALTH_FAIL=1 in_server "$here/deploy/deploy.sh" fake 2>&1); code=$?
check "server does not answer: exits non-zero" "[ $code != 0 ]"
check "server does not answer: the previous program is put back" "[ \"\$(cat '$ROOT/opt/bills/bills-backend')\" = OLD ]"
check "server does not answer: the service is restarted twice (new, then rollback)" "[ \$(restarts bills) = 2 ]"
check "server does not answer: says it rolled back" "echo \"$out\" | grep -q 'putting the previous version back'"

new_server
out=$(DRY_RUN=1 in_server "$here/deploy/deploy.sh" fake 2>&1); code=$?
check "dry run: exits 0 and stops before the server" "[ $code = 0 ] && echo \"$out\" | grep -q 'dry run'"
check "dry run: touches nothing" "[ \"\$(cat '$ROOT/opt/bills/bills-backend')\" = OLD ] && [ ! -s '$ROOT/calls.log' ]"

# ---- install-tools.sh ----
echo "== install-tools.sh"

new_server; fake_tools
out=$(in_server "$here/deploy/install-tools.sh" fake 2>&1); code=$?
check "happy path: exits 0" "[ $code = 0 ]"
check "happy path: every tool is replaced" "for t in caddy litestream rclone age age-keygen; do head -c 3 '$ROOT/bin/'\$t | grep -q '#!/' || exit 1; done"
check "happy path: the old tools are kept" "[ \"\$(cat '$ROOT/opt/bills/tools.prev/caddy')\" = 'OLD caddy' ]"
check "happy path: Caddy's service points at the new binary" "grep -q 'ExecStart=$ROOT/bin/caddy run' '$ROOT/units/caddy.service.d/10-patched-binary.conf'"
check "happy path: Caddy was validated as the caddy user" "grep -q '^runuser -u caddy' '$ROOT/calls.log'"
check "happy path: the hostname comes from the Caddyfile" "grep -q 'test.example.com:443:127.0.0.1' '$ROOT/calls.log'"
check "happy path: Caddy and Litestream restarted once each" "[ \$(restarts caddy) = 1 ] && [ \$(restarts litestream) = 1 ]"
check "happy path: the packaged age and rclone are removed" "grep -q 'apt-get .*remove age rclone' '$ROOT/calls.log'"
check "happy path: prints the installed versions" "echo \"$out\" | grep -q 'caddy      NEW'"

new_server; fake_tools
out=$(TEST_CADDY_VALIDATE_FAIL=1 in_server "$here/deploy/install-tools.sh" fake 2>&1); code=$?
check "Caddy rejects the config: exits non-zero" "[ $code != 0 ]"
check "Caddy rejects the config: says the packaged Caddy stays" "echo \"$out\" | grep -q 'leaving the packaged Caddy in use'"
check "Caddy rejects the config: no service change, no restart" "[ ! -e '$ROOT/units/caddy.service.d/10-patched-binary.conf' ] && [ \$(restarts caddy) = 0 ]"

new_server; fake_tools
out=$(TEST_CADDY_ANSWER=000 in_server "$here/deploy/install-tools.sh" fake 2>&1); code=$?
check "Caddy does not come back: exits non-zero" "[ $code != 0 ]"
check "Caddy does not come back: the override is removed (back to the packaged Caddy)" "[ ! -e '$ROOT/units/caddy.service.d/10-patched-binary.conf' ]"
check "Caddy does not come back: restarted twice (new, then rollback)" "[ \$(restarts caddy) = 2 ]"
check "Caddy does not come back: Litestream is left alone" "[ \$(restarts litestream) = 0 ]"

new_server; fake_tools
out=$(TEST_LITESTREAM_ERR=1 in_server "$here/deploy/install-tools.sh" fake 2>&1); code=$?
check "Litestream logs errors: exits non-zero" "[ $code != 0 ]"
check "Litestream logs errors: says where the old binary is" "echo \"$out\" | grep -q 'tools.prev'"

new_server; fake_tools
out=$(TEST_LITESTREAM_OFF=1 in_server "$here/deploy/install-tools.sh" fake 2>&1); code=$?
check "first install (Litestream not set up yet): exits 0" "[ $code = 0 ]"
check "first install: Litestream is not restarted" "[ \$(restarts litestream) = 0 ]"

new_server; fake_tools
echo tamper >>"$ROOT/tools/caddy"
out=$(in_server "$here/deploy/install-tools.sh" fake 2>&1); code=$?
check "tampered build: refused before anything is copied" "[ $code != 0 ] && [ \"\$(cat '$ROOT/bin/caddy')\" = 'OLD caddy' ] && [ ! -s '$ROOT/calls.log' ]"

new_server; fake_tools
out=$(TEST_CORRUPT=litestream in_server "$here/deploy/install-tools.sh" fake 2>&1); code=$?
check "corrupted copy: exits non-zero before installing" "[ $code != 0 ]"
check "corrupted copy: says nothing was installed" "echo \"$out\" | grep -q 'nothing was installed'"
check "corrupted copy: no tool was replaced" "[ \"\$(cat '$ROOT/bin/caddy')\" = 'OLD caddy' ] && [ \$(restarts caddy) = 0 ]"

echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
