#!/usr/bin/env bash
# Builds the third party programs the server runs (Caddy, Litestream, rclone, age) from
# their released source, with the Go version pinned in go.mod and the newest versions of
# the libraries they use, then scans every result for known vulnerabilities.
#
# Why: the programs you download or install from a package manager are built by their
# authors with whatever Go and libraries they had on release day, and known flaws in those
# (HTTP/2, certificate handling, SSH, gRPC) are only fixed in the next release, weeks later.
# Rebuilding from the same source with the newest fixes closes that gap now.
#
#   deploy/build-tools.sh              # builds into build/tools/ (Linux amd64)
#   TARGET_ARCH=arm64 deploy/build-tools.sh
#
# Needs: go (any recent one; it fetches the pinned toolchain), git is not needed, network.
# The source comes through the Go module proxy and is checked against Go's checksum
# database, so a tampered copy is refused. Versions are set below; change them to upgrade.
# The result is checked with govulncheck (installed on demand) and the script says so
# loudly if anything known remains.
set -euo pipefail

CADDY=${CADDY:-v2.11.7}
LITESTREAM=${LITESTREAM:-v0.5.17}
RCLONE=${RCLONE:-v1.75.2}
AGE=${AGE:-v1.3.2}
ARCH=${TARGET_ARCH:-amd64}

here=$(cd "$(dirname "$0")/.." && pwd)
OUT=${OUT:-$here/build/tools}
TOOLCHAIN=$(awk '/^toolchain /{print $2}' "$here/go.mod")
: "${TOOLCHAIN:?no toolchain line in go.mod}"
export GOTOOLCHAIN=$TOOLCHAIN GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 GOFLAGS=-mod=mod
mkdir -p "$OUT"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
echo "building with $(go version | cut -d' ' -f3), for linux/$ARCH, into $OUT"

# Libraries that the tools pull in and that have had security fixes. Each is raised to
# its newest release when the tool uses it; the build and the scan then say if that worked.
BUMP="golang.org/x/net golang.org/x/crypto golang.org/x/text golang.org/x/sys golang.org/x/oauth2 google.golang.org/grpc google.golang.org/protobuf github.com/klauspost/compress go.opentelemetry.io/otel go.opentelemetry.io/otel/sdk go.opentelemetry.io/otel/trace go.opentelemetry.io/otel/metric"

bump() { # raise the listed libraries that this module uses
	local m
	for m in $BUMP; do
		if go list -m "$m" >/dev/null 2>&1; then go get "$m@latest" >/dev/null 2>&1 || echo "  (could not raise $m)"; fi
	done
	go mod tidy >/dev/null 2>&1 || true
}

# fetch copies a released module's source (verified by Go's checksum database) into $work/<name>.
fetch() { # name module version
	local dir
	dir=$(env -u GOOS -u GOARCH CGO_ENABLED=1 go mod download -json "$2@$3" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Dir"])')
	rm -rf "${work:?}/$1" && cp -R "$dir" "$work/$1" && chmod -R u+w "$work/$1"
}

echo "== caddy $CADDY"
mkdir -p "$work/caddy" && cd "$work/caddy"
cat >main.go <<'EOF'
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

func main() { caddycmd.Main() }
EOF
go mod init caddybuild >/dev/null 2>&1
go get "github.com/caddyserver/caddy/v2@$CADDY" >/dev/null 2>&1
# Go picks its default runtime behaviours from the "go" line of the program's own module. This module
# is ours, so give it the line Caddy's authors use, to build with the behaviours they test against.
go mod edit -go="$(go list -m -f '{{.GoVersion}}' github.com/caddyserver/caddy/v2)"
bump
go build -trimpath -ldflags="-s -w" -o "$OUT/caddy" .

echo "== litestream $LITESTREAM"
fetch litestream github.com/benbjohnson/litestream "$LITESTREAM"
cd "$work/litestream"
bump
go build -trimpath -ldflags="-s -w -X main.Version=$LITESTREAM" -o "$OUT/litestream" ./cmd/litestream

echo "== rclone $RCLONE"
fetch rclone github.com/rclone/rclone "$RCLONE"
cd "$work/rclone"
bump
go build -trimpath -ldflags="-s -w -X github.com/rclone/rclone/fs.Version=$RCLONE" -tags noselfupdate -o "$OUT/rclone" .

echo "== age $AGE"
fetch age filippo.io/age "$AGE"
cd "$work/age"
bump
go build -trimpath -ldflags="-s -w -X main.Version=$AGE" -o "$OUT/age" ./cmd/age
go build -trimpath -ldflags="-s -w -X main.Version=$AGE" -o "$OUT/age-keygen" ./cmd/age-keygen

echo "== the Go language version each tool is built with (the authors' own go.mod line, which sets its default behaviours)"
for d in caddy litestream rclone age; do printf '  %-10s go %s\n' "$d" "$(cd "$work/$d" && go list -m -f '{{.GoVersion}}')"; done

echo "== scanning the results for known vulnerabilities"
unset GOOS GOARCH CGO_ENABLED GOFLAGS
if ! command -v govulncheck >/dev/null; then
	GOTOOLCHAIN=$TOOLCHAIN go install golang.org/x/vuln/cmd/govulncheck@latest
	gopath=$(go env GOPATH)
	export PATH="$PATH:$gopath/bin"
fi
# GO-2026-5932 says golang.org/x/crypto/openpgp is "unmaintained, unsafe by design". It has no fix,
# every upstream build carries it, and it is only a warning about using OpenPGP at all, so it is the
# one accepted exception. Anything else found is a failure.
ACCEPTED=${ACCEPTED:-GO-2026-5932}
bad=0
for f in "$OUT"/*; do
	[ -f "$f" ] || continue
	ids=$(govulncheck -mode=binary "$f" 2>&1 | grep -oE 'GO-[0-9]+-[0-9]+' | sort -u || true)
	left=$(echo "$ids" | grep -vxE "$(echo "$ACCEPTED" | tr ' ' '|')" | tr '\n' ' ' || true)
	if [ -z "$left" ]; then printf '  %-12s clean (accepted exception: %s)\n' "$(basename "$f")" "$(echo "$ids" | tr '\n' ' ')"; else printf '  %-12s STILL VULNERABLE: %s\n' "$(basename "$f")" "$left"; bad=1; fi
done
rm -f "$OUT/SHA256SUMS"
(cd "$OUT" && shasum -a 256 age age-keygen caddy litestream rclone >SHA256SUMS)
if [ $bad = 1 ]; then echo "SOME KNOWN VULNERABILITIES REMAIN: see above; do not ship those binaries until you have read them" >&2; exit 1; fi
echo "all clean. Checksums in $OUT/SHA256SUMS"
