#!/usr/bin/env bash
# Lets only Cloudflare reach the web ports. With Cloudflare in front, nobody else
# has a reason to connect to the server's address directly, and closing it to
# everyone else means attackers cannot go around Cloudflare's protection.
#
# Run as root on the server. Cloudflare's address list rarely changes; run it
# again if Cloudflare announces a change (https://www.cloudflare.com/ips/).
# SSH (port 22) is not touched. DRY_RUN=1 prints the commands without running them.
set -euo pipefail

run() { if [ -n "${DRY_RUN:-}" ]; then echo "+ $*"; else "$@"; fi; }

v4=$(curl -fsS https://www.cloudflare.com/ips-v4)
v6=$(curl -fsS https://www.cloudflare.com/ips-v6)
[ -n "$v4" ] && [ -n "$v6" ] || { echo "could not download Cloudflare's address lists" >&2; exit 1; }

# Close the ports to everyone first, then open them to Cloudflare only.
run ufw delete allow 80/tcp >/dev/null 2>&1 || true
run ufw delete allow 443/tcp >/dev/null 2>&1 || true
for cidr in $v4 $v6; do
	run ufw allow from "$cidr" to any port 443 proto tcp comment cloudflare
done
run ufw status numbered
