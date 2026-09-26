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
#
# PORTABILITY: no GNU-make-only flags. FreeBSD's /usr/bin/make is BSD
# make, which rejects --no-print-directory (and every other GNU
# long option) by printing its usage and exiting non-zero - so a
# verification script that passes one fails on the platform it is
# meant to be verifying, while passing on the Mac. That is the same
# class of bug as the $(shell) stamp: verified in one place, broken
# in the other.
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

# The shape the Makefile actually produces, computed the way the
# Makefile computes it - through the script, not by hand - so this
# exercises the same path a deploy takes. Before this script existed the
# hand-written form below was the only thing tested, and it passed
# happily while every real build on FreeBSD stamped nothing at all.
echo "=== ldflags the Makefile would use ==="
scripts/build-ldflags.sh
scripts/build-ldflags.sh --id
scripts/build-ldflags.sh --why
make check-ldflags
echo

# Which changes make a build dirty is a rule with a permissive failure
# mode, so it gets its own tests rather than being trusted because it
# looks right. They run against throwaway fixture repositories, so this
# never modifies the checkout it is run from.
echo "=== which worktree changes make a build dirty ==="
scripts/test-worktree-state.sh
echo

for b in $BINS; do
  echo "=== $b (clean stamp) ==="
  go build -trimpath -buildvcs=false -ldflags "$(scripts/build-ldflags.sh)" -o "$OUT/$b" ./cmd/"$b"
  "$OUT/$b" -version 2>&1 | head -3
  echo
done

for b in $BINS; do
  echo "=== $b (dirty stamp) ==="
  go build -trimpath -buildvcs=false -ldflags "-X $PKG.BuildID=9c43262d358a-dirty -X $PKG.BuildTime=2026-09-26T11:52:03-04:00 -X $PKG.GitCommit=$COMMIT" -o "$OUT/$b" ./cmd/"$b"
  "$OUT/$b" -version 2>&1 | head -3
  echo
done

for b in $BINS; do
  echo "=== $b (partial stamp: id only, no date) ==="
  go build -trimpath -buildvcs=false -ldflags "-X $PKG.BuildID=9c43262d358a" -o "$OUT/$b" ./cmd/"$b"
  "$OUT/$b" -version 2>&1 | head -1
  echo
done

echo "=== an unstamped build must be refused by check-stamped ==="
go build -trimpath -buildvcs=false -o "$OUT/raftd" ./cmd/raftd
if (cd "$OUT" && sh -c 'id=`./raftd -version 2>&1 | grep -o "build=[^ ]*" | head -1 | cut -d= -f2`; [ "$id" = unknown ] && echo "correctly detected: $id" || { echo "NOT detected, got $id"; exit 1; }'); then
  echo
else
  echo "check-stamped would have shipped an unstamped binary" >&2
  exit 1
fi

echo
echo "=== the same commit built twice must be the same bytes ==="
make check-reproducible
