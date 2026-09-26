#!/bin/sh
# Prove which worktree changes make a build dirty, and - just as
# importantly - which ones do not.
#
# WHY A FIXTURE REPO RATHER THAN THIS CHECKOUT
#
# Every case here needs a tree in a specific state, and the states
# include "a tracked file is modified" and "a .gitignore pattern hides a
# file". Producing those in the real checkout to test them, then putting
# them back, is a test that can leave the tree wrong if it is ever
# interrupted - and a test that can leave an operator's working tree
# wrong is worse than no test, because it costs them real work when it
# goes wrong. So each case gets its own throwaway git repository under a
# temporary directory, and the real checkout is never touched at all.
#
# The script under test cds to its own ../, so copying it into the
# fixture's scripts/ is all it takes to point it at the fixture. That is
# also what the cd is for: it is what makes the fixture possible without
# adding a test-only override to the script itself.
#
# Run by scripts/check-version.sh; safe and cheap to run alone.
set -e

here=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

fails=0
cases=0

fail() {
	echo "FAIL: $1" >&2
	fails=$((fails + 1))
}

commit() {
	git -C "$1" add -A
	git -C "$1" -c user.name=fixture -c user.email=fixture@example \
		commit -qm 'fixture'
}

# newrepo <name> - a minimal checkout with two nested packages, one
# tracked file in each, and everything committed, so the tree starts
# clean. There is deliberately no .go file at the repo root, which is
# true of this repository too and is what makes the root-level cases
# below meaningful.
newrepo() {
	r=$TMP/$1
	mkdir -p "$r/scripts" "$r/pkg/inner"
	cp "$here/build-ldflags.sh" "$r/scripts/build-ldflags.sh"
	chmod +x "$r/scripts/build-ldflags.sh"
	printf 'package pkg\n' >"$r/pkg/a.go"
	printf 'package inner\n' >"$r/pkg/inner/b.go"
	git -C "$r" init -q .
	commit "$r"
	echo "$r"
}

# expect_clean <repo> <description>
expect_clean() {
	cases=$((cases + 1))
	got=$("$1/scripts/build-ldflags.sh" --id)
	case "$got" in
	*-dirty)
		fail "$2: expected a clean id, got $got"
		"$1/scripts/build-ldflags.sh" --why >&2
		;;
	*)
		why=$("$1/scripts/build-ldflags.sh" --why)
		case "$why" in
		clean:*) ;;
		*) fail "$2: clean id, but --why reported: $why" ;;
		esac
		;;
	esac
}

# expect_dirty <repo> <description> [path that --why must name]
expect_dirty() {
	cases=$((cases + 1))
	got=$("$1/scripts/build-ldflags.sh" --id)
	case "$got" in
	*-dirty) ;;
	*) fail "$2: expected a dirty id, got $got" ;;
	esac
	if [ -n "${3:-}" ]; then
		why=$("$1/scripts/build-ldflags.sh" --why)
		case "$why" in
		*"$3"*) ;;
		*) fail "$2: --why did not name $3; it said: $why" ;;
		esac
	fi
}

echo "=== a committed tree is clean ==="
r=$(newrepo base)
expect_clean "$r" "freshly committed fixture"

echo "=== the built binary at the repo root does not make it dirty ==="
# This repository's own shape: `make build` drops raftd, managerd and
# friends in the repo root, .gitignore covers them, and the repo root
# holds no tracked .go file. If this ever started reporting dirty, every
# build on every Comb would carry a -dirty that means nothing.
r=$(newrepo rootbinary)
printf '/raftd\n' >"$r/.gitignore"
commit "$r"
printf 'binary\n' >"$r/raftd"
expect_clean "$r" "gitignored built binary at the repo root"

echo "=== changes outside a package dir are clean ==="
r=$(newrepo outside)
printf 'scratch\n' >"$r/notes-scratch.txt"
expect_clean "$r" "untracked file at the repo root"

r=$(newrepo outsidedir)
mkdir -p "$r/chprobe"
printf 'package chprobe\n' >"$r/chprobe/main.go"
expect_clean "$r" "untracked package dir at the repo root (a live Comb's chprobe/)"

r=$(newrepo ignoredroot)
printf '/notes.txt\n' >"$r/.gitignore"
commit "$r"
printf 'scratch\n' >"$r/notes.txt"
expect_clean "$r" "gitignored file at the repo root"

echo "=== changes inside a package dir are dirty ==="
r=$(newrepo inner-go)
printf 'package pkg\n' >"$r/pkg/scratch.go"
expect_dirty "$r" "untracked .go file in a package dir" "pkg/scratch.go"

r=$(newrepo inner-asset)
printf 'embedded\n' >"$r/pkg/extra.json"
expect_dirty "$r" "untracked embeddable asset in a package dir" "pkg/extra.json"

r=$(newrepo inner-nested)
printf 'package inner\n' >"$r/pkg/inner/scratch.go"
expect_dirty "$r" "untracked .go file in a nested package dir" "inner/scratch.go"

r=$(newrepo inner-newdir)
mkdir -p "$r/pkg/newthing"
printf 'package newthing\n' >"$r/pkg/newthing/x.go"
expect_dirty "$r" "untracked package inside a package dir" "newthing"

# Conservative on purpose. Nothing about a file named raftd inside a
# package dir can reach a binary - it is not Go, C or assembly, and
# nothing embeds it - but a rule that had to reason about file contents
# to exclude it would be one refactor away from being wrong in the
# direction that actually costs something. So this stays dirty.
r=$(newrepo inner-ignored)
printf '/pkg/raftd\n' >"$r/.gitignore"
commit "$r"
printf 'binary\n' >"$r/pkg/raftd"
expect_dirty "$r" "gitignored file in a package dir (conservative)" "pkg/raftd"

echo "=== changes to tracked files are dirty ==="
r=$(newrepo modified)
printf 'package pkg\n\n// edited\n' >"$r/pkg/a.go"
expect_dirty "$r" "modified tracked .go file" "pkg/a.go"

r=$(newrepo staged)
printf 'package pkg\n\n// staged\n' >"$r/pkg/a.go"
git -C "$r" add pkg/a.go
expect_dirty "$r" "staged tracked .go file" "pkg/a.go"

r=$(newrepo deleted)
rm -f "$r/pkg/inner/b.go"
expect_dirty "$r" "deleted tracked .go file" "b.go"

r=$(newrepo renamed)
git -C "$r" mv pkg/inner/b.go pkg/inner/c.go
expect_dirty "$r" "renamed tracked .go file" "c.go"

# These three are the cases that catch the worst possible bug in this
# rule: a tracked change being read as if it were untracked, and then
# dropped for sitting outside a package dir. The Makefile and a
# docs/*.md are not Go, but the Makefile is very much a build input, and
# a rule that reported "clean" here would be reporting it about the file
# that decides how every binary gets stamped.
r=$(newrepo makefile)
printf 'all:\n\techo original\n' >"$r/Makefile"
commit "$r"
printf 'all:\n\techo changed\n' >"$r/Makefile"
expect_dirty "$r" "modified tracked Makefile at the repo root" "Makefile"

r=$(newrepo doc)
mkdir -p "$r/docs"
printf 'notes\n' >"$r/docs/adr.md"
commit "$r"
printf 'notes, revised\n' >"$r/docs/adr.md"
expect_dirty "$r" "modified tracked markdown outside any package dir" "adr.md"

r=$(newrepo rootfile)
printf 'hello\n' >"$r/README.md"
commit "$r"
rm -f "$r/README.md"
expect_dirty "$r" "deleted tracked file at the repo root" "README.md"

echo "=== an explicitly pinned BUILD_ID is not a worktree question at all ==="
r=$(newrepo pinned)
printf 'package pkg\n\n// edited\n' >"$r/pkg/a.go"
cases=$((cases + 1))
got=$(BUILD_ID=release-1.0 "$r/scripts/build-ldflags.sh" --id)
if [ "$got" != release-1.0 ]; then
	fail "pinned BUILD_ID was overridden by the worktree: got $got"
fi

echo
if [ "$fails" -ne 0 ]; then
	echo "$fails of $cases worktree-state checks FAILED" >&2
	exit 1
fi
echo "all $cases worktree-state checks passed"
