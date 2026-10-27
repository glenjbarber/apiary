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
#	scripts/build-ldflags.sh --why    # what made the worktree dirty, if so
#
# Overridable, for a release build or a test:
#
#	BUILD_ID=release-1.0 BUILD_TIME=2026-09-26T00:00:00Z \
#	    scripts/build-ldflags.sh
#
# The Makefile passes BUILD_ID/BUILD_TIME through when they are set, so
# both `make build BUILD_ID=x` and `BUILD_ID=x make build` work.
#
# VERSION is overridable the same way, and is the release version the
# binary will report. See "THE RELEASE VERSION" below for how it
# defaults and why it is derived from git rather than from the clock.
set -e

PKG=github.com/glenjbarber/apiary/internal/buildinfo

# Run from the repository root regardless of the caller's directory, so
# the id describes this checkout and not whatever happens to be above it.
cd "$(dirname "$0")/.."

# THE RELEASE VERSION
#
# A fourth value is stamped: the release version, in buildinfo.Version.
# It is a human-facing label - "which release is this" - and it is the
# one field an operator reads to decide whether the binary on a Comb is
# the one they meant to put there.
#
# It was declared and read by buildinfo, and never set by anything, for
# as long as buildinfo existed. That is not a cosmetic gap: a released
# binary that cannot say which release it is has no way to be checked
# against the tag it claims to come from, and versioncheck - which
# exists to answer exactly "is the running binary the build I
# installed?" - has nothing to compare.
#
# It is stamped HERE rather than in the Makefile, for the same reason
# the id and the date are: this script is the one place that decides
# what a build says about itself, so there is one place to look when
# the answer is wrong.
#
# The default is derived from the git tag, when the tree carries one.
# A build of a tagged commit therefore reports the tag's version, and
# a build of an untagged commit reports "devel" rather than nothing -
# because a binary that says nothing is a binary an operator has to
# guess about.
#
# It is NOT derived from the clock, for the same reason the id is not:
# a value that changed on every build would make two builds of one
# commit differ, and would break the reproducibility claim that
# `make check-reproducible` proves. The version is a property of the
# source, so it is read from the source's own tag.
#
# An explicit VERSION wins over the derived one, which is what a
# release build does:
#
#	VERSION=1.0.0-alpha scripts/build-ldflags.sh
#
# and the Makefile passes VERSION through on both spellings, like
# BUILD_ID.
#
# WHY NOT THE TAG VERBATIM
#
# The tag is "v1.0.0-alpha" and the version is "1.0.0-alpha". The
# leading "v" is dropped because buildinfo's own comment says the
# field is a "release or version label" and the repo's convention
# everywhere else is to write a version without it - the release
# process document calls the release "1.0.0-alpha" and calls the tag
# "v1.0.0-alpha". Keeping the "v" in one place and not the other is
# the kind of small inconsistency that makes a reader wonder whether
# there are two versions.

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

# worktree_findings prints one `git status --porcelain` line per change
# that can actually reach the compiled binaries. Empty output means the
# worktree is clean. Called for its exit status, and directly by --why.
#
# WHY NOT JUST "git status --porcelain IS NON-EMPTY"
#
# Because the permissive direction is the one mistake this scheme cannot
# recover from. If -dirty is missing when it should be there, two
# different builds carry the same clean id and versioncheck reports them
# as the same build - a lie no later check can detect. So the rule below
# is deliberately narrower than "anything is modified", and the
# narrowness is provable rather than hopeful:
#
#   1. Any change to a TRACKED file reaches the build. Nothing to
#      decide: count it.
#   2. An untracked or gitignored file cannot reach the build unless its
#      own directory is a real Go package - one with at least one
#      tracked .go file in it. An import graph is built out of import
#      statements, and every one of those lives in a tracked .go file;
#      under rule 1 those files are exactly HEAD's, so every package in
#      the graph is a package HEAD already named. A directory holding no
#      tracked .go file is not one of them, and an untracked .go file
#      sitting there is unreachable: `go build ./cmd/x` never opens it
#      and nothing in HEAD can import it.
#
# Rule 2 is why this repository's own scratch does not count. bugs.txt,
# misc*.txt and a scratch chprobe/ live at the repo root, which has no
# tracked .go file, so they cannot change a binary. A rule that counted
# them would mark essentially every build -dirty, and a -dirty on every
# build is a warning nobody reads - the same dead-signal failure as the
# empty stamp this file exists to prevent.
#
# The residual hole is a repository that is already broken: a tracked
# file importing a package that is not tracked at all, so a fresh clone
# cannot build it. Nothing keyed on git can distinguish that from a
# clean tree, and it is a broken checkout rather than an identity
# problem. `go build` failing on a fresh clone is what catches it.
#
# Fails closed everywhere: if git cannot answer, this reports the reason
# rather than concluding "clean".
worktree_findings() {
	if [ "$git_readable" -ne 1 ]; then
		echo "?? (git cannot read this checkout, so its state is unknown)"
		return 0
	fi
	if ! status=$(git -c core.quotePath=false status --porcelain \
			--ignored=matching 2>/dev/null); then
		echo "?? (git status failed, so the worktree's state is unknown)"
		return 0
	fi
	# A repository with no tracked .go file cannot be this one, and
	# without the package list rule 2 below would reject everything.
	# Report the doubt rather than guess clean.
	pkgdirs=$(git ls-files '*.go' 2>/dev/null | while read -r f ; do
			dirname "$f"
		done | sort -u)
	if [ -z "$pkgdirs" ]; then
		echo "?? (no tracked .go files found, so the worktree cannot be judged)"
		return 0
	fi
	printf '%s\n' "$status" | while IFS= read -r line ; do
		case "$line" in
		"")
			continue
			;;
		"?? "*|"!! "*)
			# Untracked or ignored: reaches the build only from
			# inside a package dir (rule 2). The pattern is quoted
			# so its ?s are literal - an unquoted "?? *" would be a
			# glob matching any two characters, and so would swallow
			# " M docs/adr.md" and quietly drop a tracked change
			# for sitting outside a package dir.
			p=$(printf '%s' "$line" | cut -c4-)
			case "$p" in
			\"*)
				# git quoted the path, so it holds something this
				# line-oriented reading cannot split safely - a
				# newline, a quote, a backslash. Report it rather
				# than guess: this is the one place a wrong guess
				# could hide a change that matters.
				printf '%s\n' "$line"
				;;
			*)
				d=$(dirname "$p")
				if printf '%s\n' "$pkgdirs" | grep -qxF "$d"; then
					printf '%s\n' "$line"
				fi
				;;
			esac
			;;
		*)
			# A tracked file changed: always reaches the build,
			# wherever it lives.
			printf '%s\n' "$line"
			;;
		esac
	done
}

# --why answers the only question an operator ever has about a -dirty
# build: what made it dirty? It reports what the stamp was actually
# decided on, which is a subset of `git status` - see worktree_findings
# for why - and it never dies, because it exists to explain rather than
# to refuse.
if [ "${1:-}" = "--why" ]; then
	why=$(worktree_findings)
	if [ -n "$why" ]; then
		printf '%s\n' "$why"
	else
		echo "clean: nothing in this worktree can change a compiled binary."
	fi
	exit 0
fi

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
	if [ -n "$(worktree_findings)" ]; then
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

version=${VERSION:-}
if [ -z "$version" ] && [ "$git_readable" -eq 1 ]; then
	# `git describe --tags --exact-match` prints nothing (and exits
	# non-zero) when HEAD carries no tag, which is the ordinary case
	# for a developer's build. The "devel" fallback below covers it.
	tag=$(git describe --tags --exact-match HEAD 2>/dev/null || echo "")
	if [ -n "$tag" ]; then
		version=${tag#v}
	else
		version=devel
	fi
fi
[ -n "$version" ] || version=unknown

case "$id$time$commit$version" in
*' '*) echo "build-ldflags: values must not contain spaces: [$id] [$time] [$commit] [$version]" >&2; exit 1 ;;
esac

if [ "${1:-}" = "--id" ]; then
	echo "$id"
	exit 0
fi

if [ "${1:-}" = "--version" ]; then
	echo "$version"
	exit 0
fi

# No inner quotes: go splits -ldflags on spaces and honours quotes, and
# none of these four values can contain a space (checked above).
printf -- '-X %s.BuildID=%s -X %s.BuildTime=%s -X %s.GitCommit=%s -X %s.Version=%s\n' \
	"$PKG" "$id" "$PKG" "$time" "$PKG" "$commit" "$PKG" "$version"
