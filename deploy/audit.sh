#!/usr/bin/env bash
# One check of everything that can go stale or be attacked. Run it monthly, and after any
# security news. Read only: it changes nothing on the server.
#
#   deploy/audit.sh [ssh-host] [https://your.host]      # defaults: bills, https://api.dizztech.com
#
# It reports: Go libraries with newer versions, known vulnerabilities in the code, newer
# releases of Caddy, Litestream, rclone and age, the server's own pending updates and service
# health, whether the tools on the server are the ones deploy/build-tools.sh built, backup
# freshness, and what the live site answers to the usual attacks.
set -uo pipefail

HOST=${1:-bills}
URL=${2:-https://api.dizztech.com}
here=$(cd "$(dirname "$0")/.." && pwd)
cd "$here" || exit 1
gopath=$(go env GOPATH)
export PATH="$PATH:$gopath/bin"
section() { printf '\n== %s\n' "$1"; }

section "1. Go libraries with a newer version"
updates=$(go list -u -m -f '{{if .Update}}{{.Path}} {{.Version}} -> {{.Update.Version}}{{end}}' all 2>/dev/null | grep -v '^$' || true)
if [ -n "$updates" ]; then echo "$updates" | sed 's/^/  /'; echo "  (update with: go get -u ./... && go mod tidy, then run the tests)"; else echo "  all up to date"; fi
echo "  Go toolchain: pinned $(awk '/^toolchain /{print $2}' go.mod), newest $(curl -s -m 15 'https://go.dev/dl/?mode=json' | python3 -c 'import sys,json; print(json.load(sys.stdin)[0]["version"])' 2>/dev/null)"

section "2. Known vulnerabilities in our code"
if command -v govulncheck >/dev/null; then govulncheck ./... 2>&1 | tail -3 | sed 's/^/  /'; else echo "  govulncheck is not installed (go install golang.org/x/vuln/cmd/govulncheck@latest)"; fi

section "3. Newer releases of the tools we build (versions pinned in deploy/build-tools.sh)"
for pair in "CADDY caddyserver/caddy" "LITESTREAM benbjohnson/litestream" "RCLONE rclone/rclone" "AGE FiloSottile/age"; do
	var=${pair% *}
	repo=${pair#* }
	pinned=$(grep -oE "^$var=\\\$\{$var:-[^}]+" deploy/build-tools.sh | sed 's/.*:-//')
	latest=$(curl -s -m 15 "https://api.github.com/repos/$repo/releases/latest" | python3 -c 'import sys,json; print(json.load(sys.stdin)["tag_name"])' 2>/dev/null)
	if [ "$pinned" = "$latest" ]; then printf '  %-10s %s (newest)\n' "$var" "$pinned"; else printf '  %-10s pinned %s, NEWEST %s  <- raise it in build-tools.sh, rebuild, install\n' "$var" "$pinned" "$latest"; fi
done

section "4. The server"
ssh -o BatchMode=yes -o ConnectTimeout=15 "$HOST" 'sudo bash -s' <<'REMOTE_SCRIPT' 2>&1 | sed 's/^/  /'
apt-get update -qq >/dev/null 2>&1
n=$(apt list --upgradable 2>/dev/null | grep -vc Listing)
echo "pending package updates: $n $( [ "$n" -gt 0 ] && echo '(run: sudo apt update && sudo apt upgrade)')"
echo "restart needed:          $( [ -f /var/run/reboot-required ] && echo 'YES (run: sudo reboot)' || echo no)"
echo "automatic updates:       $(systemctl is-enabled unattended-upgrades 2>/dev/null), last run $(stat -c %y /var/log/unattended-upgrades/unattended-upgrades.log 2>/dev/null | cut -c1-16)"
for s in bills litestream caddy fail2ban cron; do printf 'service %-12s %s\n' "$s" "$(systemctl is-active $s)"; done
echo "program version:         $(journalctl -u bills -o cat --no-pager 2>/dev/null | grep -o 'version [^ ]*' | tail -1)"
echo "tools: caddy $(caddy version | cut -d' ' -f1), litestream $(litestream version), rclone $(rclone version | head -1 | cut -d' ' -f2), age $(age --version)"
echo "disk used:               $(df -h / | awk 'NR==2{print $5}')   memory free: $(free -m | awk 'NR==2{print $7}') MB"
echo "litestream problems (24h): $(journalctl -u litestream --since '-24h' --no-pager -o cat | grep -cE 'level=(ERROR)')"
last=$(grep -c 'offsite backup ok' /var/log/bills-offsite.log 2>/dev/null); echo "offsite uploads logged:  $last (the newest at $(stat -c %y /var/log/bills-offsite.log 2>/dev/null | cut -c1-16))"
echo "failed logins banned:    $(fail2ban-client status sshd 2>/dev/null | awk '/Total banned/{print $NF}')"
REMOTE_SCRIPT

section "5. Are the tools on the server the ones we built?"
if [ -f build/tools/SHA256SUMS ]; then
	ssh -o BatchMode=yes "$HOST" 'cd /usr/local/bin && sha256sum caddy litestream rclone age age-keygen' 2>/dev/null | sort >/tmp/audit.server
	(cd build/tools && shasum -a 256 age age-keygen caddy litestream rclone) | sort >/tmp/audit.local
	if diff -q /tmp/audit.server /tmp/audit.local >/dev/null; then echo "  yes, byte for byte"; else echo "  DIFFERENT from build/tools: rebuild and reinstall, or investigate"; fi
	rm -f /tmp/audit.server /tmp/audit.local
else
	echo "  no build/tools here (run deploy/build-tools.sh to create it)"
fi

section "6. What the live site answers to the usual attacks"
"$here/deploy/probe.sh" "$URL" | sed 's/^/  /'
