#!/usr/bin/env bash
# Prints the version to stamp into a build, from the git tags of the repository it is run in:
#
#   1.0.0                  HEAD is exactly the tagged release v1.0.0 (a changelog commit: what should be deployed)
#   1.0.0-3-g1a2b3c4       3 commits after v1.0.0: work that has not been released yet
#   1a2b3c4                no release tag exists at all
#   <any of the above>-uncommitted   the folder also has uncommitted changes
#
# Used by deploy.sh; deploy/tests/run.sh runs it in throwaway repositories.
set -euo pipefail

v=$(git describe --tags --match 'v[0-9]*' --always 2>/dev/null || echo unknown)
v=${v#v}
if ! git diff --quiet HEAD -- 2>/dev/null; then
	v="$v-uncommitted"
fi
echo "$v"
