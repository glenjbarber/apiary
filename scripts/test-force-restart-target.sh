#!/bin/sh
# test-force-restart-target.sh - prove what `make force-restart` actually
# does, by running the real target with stand-ins for the host commands
# it shells out to.
#
# WHY THIS RUNS THE TARGET INSTEAD OF READING IT
#
# The assertion this target used to make was `service apiary_$S
# status`, and on the Combs that call is not a measurement: every
# apiary_* rc.d script reports not running, on every check, while all
# four daemons are demonstrably up and listening (sockstat is
# authoritative and disagrees with all of them). So the old check could
# only ever reach its own failure branch - on the first service, every
# time. It restarted managerd, burned the full 15s, printed the failure
# and exited 1 having never touched raftd, leaving behind a
# pending-restart record for a raftd restart that never happened: the
# exact half-restarted managerd/raftd pair this target exists to
# prevent, produced by the target itself.
#
# That defect is invisible to any test that greps the Makefile for the
# right string, because the string was right - it was the host command
# that lied. A test that asserts "the Makefile no longer says
# `service ... status`" would have passed against a Makefile that said
# `sockstat -4 -l | grep -q ":17700"` while sockstat printed a
# different column layout, and would equally have passed against a
# check that matched a substring of an unrelated port. So these cases
# execute the actual target, with `service`, `sockstat`, `sh` and
# `sleep` replaced by stand-ins that append to one ordered log, and one
# of which lies about status exactly as the real one does on a Comb.
#
# The log is the point: it is a single ordered record of every host
# command the recipe issued, so "the recorder ran before the restart",
# "raftd was never touched after managerd timed out" and "status was
# never consulted" are all read off the same artifact rather than
# inferred from exit codes and prose.
#
# WHAT THE CASES PIN
#
#   - the honest path, with `service status` lying the whole way
#   - the port each service is confirmed on, in both directions: a
#     Comb where only raftd is listening must not satisfy managerd's
#     wait, and vice versa. A check that watched "any apiary socket"
#     or that had the two ports swapped would pass a weaker test.
#   - that the wait is a bounded poll (exactly 15 probes), not one
#     probe and not an unbounded loop
#   - that a sockstat which errors is treated as "not listening"
#     rather than as success
#   - that a port number appearing without its `host:` delimiter is
#     not a match
#   - that a timeout stops before the next service is restarted, and
#     says which services were already restarted
#   - that the forced-restart record is still written, and is still
#     written *before* the restart it describes
#   - that a service with no known listener port is refused before
#     anything at all is restarted
#
# PORTABILITY: no GNU-make-only flags - see the note at the top of
# check-version.sh for why a verification script that passes on the
# Mac and fails on the Comb is worse than no script. The target is
# invoked through $MAKE so this runs under both bmake/BSD make (what a
# Comb runs) and GNU make.
#
# Wired into scripts/check-version.sh. Needs no root, no Comb, and no
# live service: the recorder takes its paths from the environment, and
# every host command is a stub.
set -e
cd "$(dirname "$0")/.."

MAKE=${MAKE:-make}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM
STUB="$TMP/stub"
mkdir -p "$STUB"

# The stand-ins. Each appends one line to $STUB_LOG, so the log is in
# invocation order and the cases can assert on sequence, not just on
# the final exit status.
cat > "$STUB/service" <<'STUBEOF'
#!/bin/sh
# Stand-in for service(8). `status` lies, faithfully: on brood and
# drone every apiary_* rc.d script reports not running while the daemon
# is up and listening. A target that trusts this is broken on the Comb
# even though it is right in a test.
echo "service $*" >> "$STUB_LOG"
case "$2" in
status)	echo "service $1: not running." >&2 ; exit 1 ;;
*)	exit 0 ;;
esac
STUBEOF

cat > "$STUB/sockstat" <<'STUBEOF'
#!/bin/sh
# Stand-in for sockstat(8) -4 -l. STUB_MODE picks the truth it reports.
echo "sockstat $*" >> "$STUB_LOG"
row() {
	printf '%-6s %-9s %5s %2s  tcp4  127.0.0.1:%-5s  *:*\n' \
		root "$1" 4242 4 "$2"
}
case "$STUB_MODE" in
all)		row managerd 17700 ; row raftd 17600 ;;
managerd)	row managerd 17700 ;;
raftd)		row raftd 17600 ;;
none)		;;
decoy)		printf '%-6s %-9s %5s %2s  tcp4  127.0.0.1.%-5s  *:*\n' \
			root odd 4242 4 17700 ;;
broken)	exit 1 ;;
*)		echo "stub sockstat: no such STUB_MODE: $STUB_MODE" >&2 ; exit 2 ;;
esac
exit 0
STUBEOF

cat > "$STUB/sleep" <<'STUBEOF'
#!/bin/sh
# The 15s budget is asserted by probe count, not by wall clock, so the
# failing cases do not spend 15 seconds each.
exit 0
STUBEOF

cat > "$STUB/sh" <<'STUBEOF'
#!/bin/sh
# The recipe invokes the recorder as `sh scripts/...`, so intercepting
# `sh` is what puts the recorder into the ordered log and makes
# "recorded before restarted" assertable. Delegated to the real shell
# so the recorder still runs for real, against fixture paths.
echo "sh $*" >> "$STUB_LOG"
exec /bin/sh "$@"
STUBEOF

chmod +x "$STUB"/service "$STUB"/sockstat "$STUB"/sleep "$STUB"/sh

fails=0
cases=0

check() { # check <label> <expected> <actual>
	cases=$((cases + 1))
	if [ "$2" = "$3" ] ; then
		echo "  ok   $1"
	else
		echo "  FAIL $1"
		echo "       expected: $2"
		echo "       actual:   $3"
		fails=$((fails + 1))
	fi
}

is() { # is <label> <0|1> <observed 0|1>
	cases=$((cases + 1))
	if [ "$2" = "$3" ] ; then
		echo "  ok   $1"
	else
		echo "  FAIL $1"
		fails=$((fails + 1))
	fi
}

failed() { # failed <label> <status> - non-zero, and specifically not a
	# pinned value. A failed recipe's exit status belongs to make, not
	# to the recipe: BSD make (what a Comb runs) exits 1, GNU make exits
	# 2. Pinning either would fail this script on the other platform
	# while the target itself behaved identically on both.
	cases=$((cases + 1))
	if [ "$2" -ne 0 ] ; then
		echo "  ok   $1"
	else
		echo "  FAIL $1"
		echo "       make exited 0 on a target that must have failed"
		fails=$((fails + 1))
	fi
}

section() { # section <exact line> - that line and the one after it
	printf '%s\n' "$OUT" | sed -n "/$(printf '%s' "$1" | sed 's/[][\.*^$\/]/\\&/g')\$/,+1p"
}

has() { # has <label> <substring>  - against this case's target output
	cases=$((cases + 1))
	if printf '%s\n' "$OUT" | grep -qF -- "$2" ; then
		echo "  ok   $1"
	else
		echo "  FAIL $1"
		echo "       not in the target's output: $2"
		printf '%s\n' "$OUT" | sed 's/^/       | /'
		fails=$((fails + 1))
	fi
}

lacks() { # lacks <label> <substring>
	cases=$((cases + 1))
	if printf '%s\n' "$OUT" | grep -qF -- "$2" ; then
		echo "  FAIL $1"
		echo "       unexpectedly in the output: $2"
		fails=$((fails + 1))
	else
		echo "  ok   $1"
	fi
}

inlog() { # inlog <label> <substring> - against the ordered host-command log
	cases=$((cases + 1))
	if grep -qF -- "$2" "$D/log" ; then
		echo "  ok   $1"
	else
		echo "  FAIL $1"
		echo "       never issued: $2"
		sed 's/^/       | /' "$D/log"
		fails=$((fails + 1))
	fi
}

notinlog() { # notinlog <label> <substring>
	cases=$((cases + 1))
	if grep -qF -- "$2" "$D/log" ; then
		echo "  FAIL $1"
		echo "       was issued, and must not have been: $2"
		sed 's/^/       | /' "$D/log"
		fails=$((fails + 1))
	else
		echo "  ok   $1"
	fi
}

countlog() { # countlog <label> <substring> -> count
	check "$1" "$3" "$(grep -cF -- "$2" "$D/log" || true)"
}

existed() { # existed <label> <path> <1|0 expected>
	if [ -e "$2" ] ; then found=1 ; else found=0 ; fi
	is "$1" "$3" "$found"
}

n=0
newcase() { # newcase <sockstat mode> [make args...]
	n=$((n + 1))
	D="$TMP/case$n"
	mkdir -p "$D/guardrail"
	STUB_LOG="$D/log"
	STUB_MODE="$1"
	: > "$D/log"
	shift
	# Fixture config so the recorder resolves a deterministic node_id
	# and writes into this case's own directory: no root, no /var/db.
	APIARY_GUARDRAIL_DIR="$D/guardrail"
	APIARY_RAFTD_JSON="$D/raftd.json"
	APIARY_MANAGERD_JSON="$D/managerd.json"
	APIARY_COMMON_JSON="$D/common.json"
	export STUB_LOG STUB_MODE
	export APIARY_GUARDRAIL_DIR APIARY_RAFTD_JSON APIARY_MANAGERD_JSON APIARY_COMMON_JSON
	printf '{"node_id":"comb-under-test"}\n' > "$APIARY_COMMON_JSON"
	if OUT=$(PATH="$STUB:$PATH" "$MAKE" force-restart "$@" 2>&1) ; then
		STATUS=0
	else
		STATUS=$?
	fi
}

restarts() { # restarts <service> -> 1 if the recipe restarted it
	if grep -qF -- "service apiary_$1 restart" "$D/log" ; then
		echo 1
	else
		echo 0
	fi
}

order() { # order -> the services restarted, in the order restarted
	sed -n 's/^service apiary_\([a-z]*\) restart$/\1/p' "$D/log" | tr '\n' ' '
}

MGR=$D # placeholder, overwritten per case; keeps set -u quiet if used
echo "=== force-restart, with a lying service(8) and no real daemons ==="

# 1. The honest path. Every service command is answered, every daemon
#    is listening, and `service ... status` lies the whole way - which is
#    precisely the situation that used to fail the target on its first
#    service, every time.
newcase all
check "exit status on the honest path" 0 "$STATUS"
check "restart order" "managerd raftd " "$(order)"
has "managerd is confirmed on 17700" "apiary_managerd (waiting for port 17700)"
has "raftd is confirmed on 17600" "apiary_raftd (waiting for port 17600)"
has "managerd is reported listening" "apiary_managerd is listening on port 17700"
has "raftd is reported listening" "apiary_raftd is listening on port 17600"
countlog "one probe per service, no waiting" "sockstat -4 -l" 2
notinlog "no 'service ... status' call is ever issued" " status"
existed "a record was written for managerd" \
	"$APIARY_GUARDRAIL_DIR/pending-restart-apiary_managerd.json" 1
existed "a record was written for raftd" \
	"$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json" 1
check "the managerd record names the voter and takes no lease" \
	'{"service":"apiary_managerd","node_id":"comb-under-test","lease_id":0}' \
	"$(cat "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_managerd.json")"
inlog "the recorder is invoked, by name" "sh scripts/record-forced-restart.sh managerd"
# Ordering matters for the same reason it does in the target: a record
# written after a restart that never happened teaches the cooldown
# nothing and leaves the pending file to be read by the *next*
# startup, which would then confirm a restart that is already over.
check "recorded before restarted, for both services" \
	'sh scripts/record-forced-restart.sh managerd
service apiary_managerd restart
sockstat -4 -l
sh scripts/record-forced-restart.sh raftd
service apiary_raftd restart
sockstat -4 -l' \
	"$(sed -n '1,6p' "$D/log")"
has "the plan is announced before anything is restarted" \
	"restart plan, each confirmed by its own listener port"
lacks "the closing advice has no unexpanded brace glob" "{managerd,raftd}"

# 2. Nothing is listening. The target must fail on managerd, and must
#    not go on to restart raftd into a node whose managerd is down.
newcase none
failed "exit status: managerd never listens" "$STATUS"
check "only managerd was touched" "managerd " "$(order)"
has "the failure names the service and its port" \
	"apiary_managerd did not open its listener port 17700 within 15s."
has "the failure says the next service was deliberately not restarted" \
	"Stopping here rather than restarting the next"
# A service that times out has still been restarted - the restart was
# issued, it is the listener that never appeared - so it belongs in the
# already-restarted list. That list is what an operator reads to work
# out which build this Comb is now half-running.
check "the failure names managerd as the one already restarted" \
	"  ALREADY RESTARTED on this Comb, this run:
    managerd" \
	"$(section 'ALREADY RESTARTED on this Comb, this run:')"
countlog "the wait is bounded at 15 probes" "sockstat -4 -l" 15
existed "a record was still written for the restart that was attempted" \
	"$APIARY_GUARDRAIL_DIR/pending-restart-apiary_managerd.json" 1
existed "no record was written for a raftd restart that never happened" \
	"$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json" 0

# 3. Only managerd came back. raftd must fail on its own port, and the
#    message must say what this Comb is now half-way into.
newcase managerd
failed "exit status: raftd never listens" "$STATUS"
check "both services were restarted before the failure" "managerd raftd " "$(order)"
has "the failure names raftd and port 17600" \
	"apiary_raftd did not open its listener port 17600 within 15s."
has "the failure lists what was already restarted" "ALREADY RESTARTED on this Comb"
# Compared whole, not by substring: the restart plan printed before the
# run also contains an indented "managerd", so a substring assertion
# here would be satisfied by the plan and would survive losing the
# list entirely - which is the exact mutant this check exists for.
check "the failure names both attempted restarts, not just managerd" \
	"  ALREADY RESTARTED on this Comb, this run:
    managerd raftd" \
	"$(section 'ALREADY RESTARTED on this Comb, this run:')"
has "the failure says the Comb is now mixed" "mix"
existed "a record was written for both attempted restarts" \
	"$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json" 1

# 4. raftd listening must not satisfy managerd's wait. A check that
#    watched for "any apiary socket", or that had the two ports
#    swapped, would sail through this.
newcase raftd
failed "exit status: only raftd listens" "$STATUS"
check "only managerd was touched" "managerd " "$(order)"
has "managerd's wait did not accept raftd's port" \
	"apiary_managerd did not open its listener port 17700 within 15s."

# 5. A sockstat that fails is not evidence of a listener. It is treated
#    as silence, which fails closed and loudly, rather than as success.
newcase broken
failed "exit status: sockstat itself errors" "$STATUS"
has "a broken sockstat is reported as a timeout, not as success" \
	"apiary_managerd did not open its listener port 17700 within 15s."

# 6. The port number appearing without its `host:` delimiter is not a
#    match. This is the case a "simplify the pattern" edit would break
#    silently, since the digits are right and only the delimiter is not.
newcase decoy
failed "exit status: only a colonless decoy is listening" "$STATUS"
has "a decoy row does not satisfy the port check" \
	"apiary_managerd did not open its listener port 17700 within 15s."

# 7. A service with no known listener port is refused before anything
#    is restarted, not discovered halfway through the list. The case
#    arm is the target's only map from a service to a port, and a
#    future third daemon must not be restarted into a guessed port.
newcase all FORCE_RESTART_SRCS="restshimd managerd"
failed "exit status: a service with no known port" "$STATUS"
has "the refusal names the service" "no known listener port for apiary_restshimd"
has "the refusal says it will not guess" "will not guess one"
has "the refusal says nothing was restarted" "NOTHING has been"
check "nothing at all was restarted" "" "$(order)"
existed "no record was written" "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_managerd.json" 0

echo ""
if [ "$fails" -eq 0 ] ; then
	echo "test-force-restart-target: $cases checks, all passing"
else
	echo "test-force-restart-target: $cases checks, $fails FAILED"
	exit 1
fi
