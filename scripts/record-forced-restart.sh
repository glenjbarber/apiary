#!/bin/sh
# record-forced-restart.sh - tell the restart guardrail that a
# force-restart is about to happen, so the cooldown learns about it.
#
# WHY THIS EXISTS
#
# `make force-restart` bypasses the ADR-0125/ADR-0103 guardrail on
# purpose: it hands `service` a restart directly, takes no lease, runs
# no quorum preflight, and coordinates with nothing. That much is
# intended and stays that way.
#
# The gap this closes is different. The guardrail's cooldown - the 600s
# window during which a second voter may not restart the same service -
# is fed by RestartRecord entries in the FSM, and the only thing that
# writes one is the confirm path. So a force-restart left no record
# anywhere, and the guardrail went on believing no restart had happened:
# an operator could force-restart raftd on one Comb and be granted a
# coordinated raftd restart on another seconds later, which is exactly
# the concurrent-restart window the cooldown exists to close. A digest
# of this, from 2026-09-27 01:53: "`make force-restart` takes no lease,
# so the 600s guardrail never learns a deployment happened."
#
# WHAT IT WRITES
#
# The same pending-restart record RestartNodeService writes before it
# restarts anything, read back by each daemon's own next-startup hook
# (cmd/managerd for managerd, cmd/raftd for raftd). The JSON field
# names, the file name and the write sequence are kept identical to
# internal/manager.PendingRestart and internal/restartplan.PendingRestart
# - the two of which already hold a test asserting that equivalence,
# because drift here would strand a raft-replicated lease with nothing
# able to release it.
#
# lease_id is 0, and that is the whole trick. applyRecordRestartCompleted
# writes the cooldown record unconditionally - "a real restart really did
# complete, regardless of whether the lease below still matches" - and
# releases a lease only on an exact lease_id AND holder_node_id match.
# So lease_id 0 informs the cooldown and can never release a lease
# someone else is holding, not even this node's own real one. Nothing
# checks for a zero lease id anywhere in the confirm path.
#
# TWO CASES WHERE IT DELIBERATELY WRITES NOTHING
#
# 1. A pending record already exists. That is a real lease in flight,
#    and Save overwrites. Clobbering it with lease_id 0 would strand
#    that lease for good: the daemon would confirm lease 0, the real
#    lease would never match, and leases have no TTL. Leave it alone and
#    say so loudly.
#
# 2. The node id cannot be read. applyAcquireRestartLease only counts a
#    record toward the cooldown when its holder is a currently-known
#    voter, so a record with a wrong or empty node_id is written and
#    never blocks anything - a silent no-op that looks like it worked.
#    Better to record nothing and say why.
#
# EXIT STATUS
#
# Always 0. This is bookkeeping on the emergency path, and the one thing
# it must never do is stop a restart an operator needs during an
# incident. Every failure is a loud message on stderr instead.

set -u

# Overridable so the test can drive it without root or a real host.
GUARDRAIL_DIR=${APIARY_GUARDRAIL_DIR:-/var/db/apiary/guardrail}
RAFTD_JSON=${APIARY_RAFTD_JSON:-/usr/local/etc/apiary/raftd.json}
MANAGERD_JSON=${APIARY_MANAGERD_JSON:-/usr/local/etc/apiary/managerd.json}
COMMON_JSON=${APIARY_COMMON_JSON:-/usr/local/etc/apiary/common.json}

warn() { echo "record-forced-restart: $*" >&2 ; }

if [ $# -ne 1 ] ; then
	warn "usage: record-forced-restart.sh <service>"
	exit 0
fi

case $1 in
	apiary_*) service=$1 ;;
	*)        service="apiary_$1" ;;
esac

pending="$GUARDRAIL_DIR/pending-restart-$service.json"

if [ -e "$pending" ] ; then
	warn "a pending-restart record already exists for $service:"
	warn "  $pending"
	warn "Leaving it untouched - overwriting it would strand whatever"
	warn "lease wrote it, since that lease has no TTL. If that is a"
	warn "stale record from a restart that never came back, clear it by"
	warn "hand once you have confirmed no lease is held."
	warn "The guardrail cooldown will NOT learn about this force-restart."
	exit 0
fi

# Same shape the Makefile's own config-writing recipes produce, so a
# plain sed is enough and jq is not a new dependency on a Comb.
node_id() {
	sed -n 's/.*"node_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$1" 2>/dev/null | head -1
}

# This is the same resolution order the daemons themselves use, and it
# has to be, because a record only counts toward the cooldown when its
# node_id matches a raft member id. managerd and raftd fall back to
# os.Hostname() when no config file sets one (cmd/managerd/main.go, and
# raftd's own node.Status().NodeID default), and ADR-0111's common.json
# sits between the per-daemon files and that fallback. A Comb whose
# raftd.json and managerd.json both omit node_id - which is what a Comb
# set up before that field was written into the config looks like - must
# still produce a record, or this script silently declines to do its one
# job on exactly those hosts.
NODEID=$( node_id "$RAFTD_JSON" )
[ -n "$NODEID" ] || NODEID=$( node_id "$MANAGERD_JSON" )
[ -n "$NODEID" ] || NODEID=$( node_id "$COMMON_JSON" )
if [ -z "$NODEID" ] ; then NODEID=$( hostname 2>/dev/null ) ; fi

if [ -z "$NODEID" ] ; then
	warn "could not determine a node_id from $RAFTD_JSON,"
	warn "$MANAGERD_JSON, $COMMON_JSON, or \`hostname\`."
	warn "Writing nothing: the guardrail only counts a restart record"
	warn "toward its cooldown when the holder is a known voter, so a"
	warn "record with no node_id would never block anything."
	warn "The guardrail cooldown will NOT learn about this force-restart."
	exit 0
fi

if ! mkdir -p "$GUARDRAIL_DIR" ; then
	warn "could not create $GUARDRAIL_DIR - writing nothing."
	warn "The guardrail cooldown will NOT learn about this force-restart."
	exit 0
fi
chmod 0700 "$GUARDRAIL_DIR" 2>/dev/null

# Atomic, matching the Go stores: temp file in the same directory, then
# rename. A half-written record read at the next startup would be a
# parse error, and a parse error is a restart that cannot confirm.
tmp="$GUARDRAIL_DIR/.pending-restart-$$.tmp"
if ! printf '{"service":"%s","node_id":"%s","lease_id":0}\n' \
	"$service" "$NODEID" > "$tmp" 2>/dev/null ; then
	rm -f "$tmp" 2>/dev/null
	warn "could not write $tmp - writing nothing."
	warn "The guardrail cooldown will NOT learn about this force-restart."
	exit 0
fi
chmod 0600 "$tmp" 2>/dev/null

if ! mv "$tmp" "$pending" ; then
	rm -f "$tmp" 2>/dev/null
	warn "could not install $pending - writing nothing."
	warn "The guardrail cooldown will NOT learn about this force-restart."
	exit 0
fi

echo "  recorded $service restart by voter $NODEID (lease_id 0, no lease taken)"
