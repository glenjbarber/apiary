#!/bin/sh
# Print the -X link-time flags that stamp a build with its identity.
#
# WHY THIS IS A SCRIPT AND NOT MAKE VARIABLES
#
# The obvious spelling is $(shell git ...) in the Makefile. That is GNU
# make syntax, and a Comb's /usr/bin/make is BSD make, which has no
# $(shell) at all: it parses $(shell git rev-parse ...) as a variable
# whose NAME is the whole string, warns "Invalid character in variable
# name", and expands to nothing. The build then succeeds with
#
#	-ldflags "-X '...buildinfo.BuildID=' -X '...buildinfo.BuildTime=' ..."
#
# i.e. every stamp set to the empty string, which is indistinguishable
# from an unstamped build. This was live on brood and drone: every
# binary built there reported
#
#	managerd build=unknown (not stamped; built without -ldflags ...)
#
# A stamp that silently fails is worse than no stamp, because it looks
# like the feature is working. So the identity is computed here, by the
# shell that make already runs, and make just passes the result through.
# Command substitution in a recipe is portable; $(shell) is not.
#
# THE ID ITSELF
#
# The id is the commit, with no clock in it, so the same commit builds
# to the same bytes and the same id every time. A -dirty suffix marks a
# worktree whose bytes the commit does not describe (untracked files
# count - they compile in too). See the Makefile for the reasoning.
#
# USAGE
#
#	scripts/build-ldflags.sh          # the -X flags, for go build -ldflags
#	scripts/build-ldflags.sh --id     # just the build id
#
# Overridable, for a release build or a test:
#
#	BUILD_ID=release-1.0 BUILD_TIME=2026-09-26T00:00:00Z \
#	    scripts/build-ldflags.sh
#
# The Makefile passes BUILD_ID/BUILD_TIME through when they are set, so
# both `make build BUILD_ID=x` and `BUILD_ID=x make build` work.
set -e

PKG=github.com/glenjbarber/apiary/internal/buildinfo

# Run from the repository root regardless of the caller's directory, so
# the id describes this checkout and not whatever happens to be above it.
cd "$(dirname "$0")/.."

# git_usable is the whole reason this script is careful. If the tree is
# a checkout that git refuses to read - the common case is "dubious
# ownership", git >= 2.35.2 refusing a repo owned by another user, which
# is what an unprivileged `make build` in a root-owned checkout hits -
# then there is no commit to name. Emitting a placeholder would be a
# lie: two unrelated builds would then carry the same id, and
# versioncheck would report them as the same build. So it stops, and
# says the one command that fixes it. ALLOW_UNIDENTIFIED=1 accepts the
# loss of identity deliberately, and is the same knob `make install`
# honours.
git_readable=0
if git rev-parse --git-dir >/dev/null 2>&1; then
	git_readable=1
fi

die_unidentified() {
	echo "build-ldflags: this tree looks like a git checkout, but git" >&2
	echo "  cannot read it, so the build cannot be tied to a commit:" >&2
	echo "    $1" >&2
	echo "  Refusing to stamp it, because two different builds would" >&2
	echo "  otherwise carry the same id and be reported as one build." >&2
	echo >&2
	echo "  Fix it with either:" >&2
	echo "    git config --global --add safe.directory $(pwd)" >&2
	echo "    chown -R \$(id -un) $(pwd)" >&2
	echo "  or accept an unidentifiable build with:" >&2
	echo "    ALLOW_UNIDENTIFIED=1 make install" >&2
	exit 1
}

[ "$git_readable" -eq 1 ] || {
	if [ -n "${ALLOW_UNIDENTIFIED:-}" ]; then
		echo "build-ldflags: WARNING - proceeding with no commit identity." >&2
	else
		die_unidentified "$(git rev-parse --git-dir 2>&1 | head -2 | tr '\n' ' ')"
	fi
}

if [ "$git_readable" -eq 1 ]; then
	short=$(git rev-parse --short=12 HEAD 2>/dev/null || echo nogit)
	commit=$(git rev-parse HEAD 2>/dev/null || true)
	# Untracked files count: git status --porcelain lists them, and an
	# untracked .go file compiles into the binary exactly as a modified
	# one does.
	if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
		dirty="-dirty"
	else
		dirty=""
	fi
else
	short="nogit"
	commit=""
	dirty=""
fi

id=${BUILD_ID:-}
[ -n "$id" ] || id="$short$dirty"

# The commit's own date, not the clock. A date is worth having in a
# startup log, and this one is a fixed property of the source, so it
# costs no reproducibility.
time=${BUILD_TIME:-}
[ -n "$time" ] || time=$(git log -1 --format=%cI 2>/dev/null || echo unknown)

case "$id$time$commit" in
*' '*) echo "build-ldflags: values must not contain spaces: [$id] [$time] [$commit]" >&2; exit 1 ;;
esac

if [ "${1:-}" = "--id" ]; then
	echo "$id"
	exit 0
fi

# No inner quotes: go splits -ldflags on spaces and honours quotes, and
# none of these three values can contain a space (checked above).
printf -- '-X %s.BuildID=%s -X %s.BuildTime=%s -X %s.GitCommit=%s\n' \
	"$PKG" "$id" "$PKG" "$time" "$PKG" "$commit"
