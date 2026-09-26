#!/bin/sh
# Decide what one build's stamped identity means to an installer.
#
# USAGE
#
#	scripts/stamp-verdict.sh <build-id>
#
# Prints one line, "<class> <reason>":
#
#	ok	the id is evidence of what these bytes are
#	dirty	the id is honest that it is NOT evidence of that
#	fail	there is no usable id at all
#
# An empty argument is a verdict, not a usage error: an empty id is what
# a binary that did not run, or would not stop for -version, looks like,
# and that is a fail.
#
# WHY dirty AND fail ARE SEPARATE CLASSES
#
# They are the two halves of the same mistake, and this script exists to
# keep them apart.
#
# "fail" is a silent failure. It is what BSD make's missing $(shell)
# produced on every Comb: the build succeeds, the -X flags arrive with
# empty values, the binary installs, and every daemon reports
# build=unknown while the entire toolchain looks healthy. An id that
# cannot be produced must stop the install, because nothing downstream
# can tell an operator that it is missing.
#
# "dirty" is the opposite: a success, reported accurately. The binary
# names its commit AND says the worktree was not clean, so the commit
# does not describe the bytes. That is strictly more information than a
# clean build carries.
#
# `make install` used to treat both as fatal, and that was worse than
# the bug it was built to catch. A -dirty build is completely normal -
# it is what every build looks like on a host with a scratch file in the
# checkout - so refusing it blocked ordinary deploys, and the only way
# through was ALLOW_UNIDENTIFIED=1. That knob also suppresses the fail
# class, so the sole remedy for a too-strict check was to disable the
# check that matters. A guard whose only escape hatch turns it off is a
# guard that gets turned off.
#
# The cost of accepting a dirty build is real and is not hidden here: two
# builds from two different dirty worktrees share one id, so the id
# cannot prove which bytes are running. buildinfo's report and
# versioncheck's compare() both say so in those words, and
# `make install` says what to run instead - sha256, which is the only
# thing that actually distinguishes two artifacts.
set -e

if [ $# -lt 1 ]; then
	echo "usage: $(basename "$0") <build-id>" >&2
	exit 2
fi

id=$1

case "$id" in
"")
	echo "fail it did not answer -version. Either the binary was built for a different host and cannot run here, or it predates -version and will not stop for it"
	;;
unknown)
	echo "fail not stamped - the -X link flags never reached this build, so every binary built this way reports build=unknown"
	;;
nogit)
	echo "fail built outside a git checkout, or git cannot read this one, so there is no commit to name"
	;;
*-dirty)
	echo "dirty stamped $id, but the worktree was not clean when it was built: the commit does not describe these bytes, and two such builds can share this id and still differ"
	;;
*)
	echo "ok stamped $id"
	;;
esac
