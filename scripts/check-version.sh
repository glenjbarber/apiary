#!/bin/sh
# Exercise -version on all five binaries, both stamped and unstamped.
set -e
cd "$(dirname "$0")/.."
PKG=github.com/glenjbarber/apiary/internal/buildinfo
OUT=/tmp/apibuild
mkdir -p "$OUT"
for b in raftd managerd frontend restshimd apiaryinstall; do
  echo "=== $b (unstamped) ==="
  go build -o "$OUT/$b" ./cmd/"$b"
  "$OUT/$b" -version 2>&1 | head -3
  echo
done
for b in raftd managerd frontend restshimd apiaryinstall; do
  echo "=== $b (stamped) ==="
  go build -ldflags "-X $PKG.BuildID=test-abc123-20260926 -X $PKG.BuildTime=2026-09-26T12:35:00Z -X $PKG.GitCommit=0123456789abcdef0123456789abcdef01234567" -o "$OUT/$b" ./cmd/"$b"
  "$OUT/$b" -version 2>&1 | head -8
  echo
done
