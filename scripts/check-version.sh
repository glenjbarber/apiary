#!/bin/sh
# Exercise -version on all five binaries across every stamp shape the
# Makefile can produce, plus the ones it must never produce a false
# claim about.
#
# The three stamped cases are the ones that matter:
#
#   clean     - what a real deploy gives you
#   dirty     - a worktree with local changes: the id no longer
#               describes the bytes, and -version has to say so
#   partial   - an id with no date, i.e. a hand-rolled ldflags line; must
#               read "built=unknown" rather than omitting the field
set -e
cd "$(dirname "$0")/.."
PKG=github.com/glenjbarber/apiary/internal/buildinfo
COMMIT=0123456789abcdef0123456789abcdef01234567
OUT=/tmp/apibuild
mkdir -p "$OUT"

BINS="raftd managerd frontend restshimd apiaryinstall"

for b in $BINS; do
  echo "=== $b (unstamped) ==="
  go build -trimpath -buildvcs=false -o "$OUT/$b" ./cmd/"$b"
  "$OUT/$b" -version 2>&1 | head -3
  echo
done

for b in $BINS; do
  echo "=== $b (clean stamp) ==="
  go build -trimpath -buildvcs=false -ldflags "-X $PKG.BuildID=9c43262d358a -X $PKG.BuildTime=2026-09-26T11:52:03-04:00 -X $PKG.GitCommit=$COMMIT" -o "$OUT/$b" ./cmd/"$b"
  "$OUT/$b" -version 2>&1 | head -8
  echo
done

for b in $BINS; do
  echo "=== $b (dirty stamp) ==="
  go build -trimpath -buildvcs=false -ldflags "-X $PKG.BuildID=9c43262d358a-dirty -X $PKG.BuildTime=2026-09-26T11:52:03-04:00 -X $PKG.GitCommit=$COMMIT" -o "$OUT/$b" ./cmd/"$b"
  "$OUT/$b" -version 2>&1 | head -8
  echo
done

for b in $BINS; do
  echo "=== $b (partial stamp: id only, no date) ==="
  go build -trimpath -buildvcs=false -ldflags "-X $PKG.BuildID=9c43262d358a" -o "$OUT/$b" ./cmd/"$b"
  "$OUT/$b" -version 2>&1 | head -1
  echo
done

echo "=== the same commit built twice must be the same bytes ==="
make --no-print-directory check-reproducible
