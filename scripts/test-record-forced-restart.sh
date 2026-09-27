#!/bin/sh
# Prove what scripts/record-forced-restart.sh writes, and - at least as
# importantly - what it refuses to write.
#
# WHY THESE CASES MATTER
#
# This script's only job is to leave the guardrail a truthful record of
# a force-restart. Two ways it could lie quietly:
#
#   - Write a record that never blocks anything. applyAcquireRestartLease
#     counts a RestartRecord toward the cooldown only when its holder is
#     a currently-known voter, so a wrong or empty node_id produces a
#     record that looks like success and does nothing. That is the case
#     most worth testing, because it is invisible from the outside.
#
#   - Overwrite a real lease's pending record. Save overwrites, leases
#     have no TTL, and the daemon would then confirm lease_id 0 and
#     never match the real lease - stranding it permanently. The
#     refusal to clobber is load-bearing, not tidiness.
#
# WHY ENV OVERRIDES RATHER THAN A FIXTURE TREE
#
# record-forced-restart.sh takes its state dir and config paths from the
# environment, so these cases need only a temporary directory. Nothing
# here touches the real checkout, and nothing needs root.
#
# Run by scripts/check-version.sh; safe and cheap to run alone.
set -e

here=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

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

fresh() { # fresh -> empty guardrail dir and config paths under $TMP/caseN
	n=$((n + 1))
	D="$TMP/case$n"
	mkdir -p "$D"
	APIARY_GUARDRAIL_DIR="$D/guardrail"
	APIARY_RAFTD_JSON="$D/raftd.json"
	APIARY_MANAGERD_JSON="$D/managerd.json"
	APIARY_COMMON_JSON="$D/common.json"
	export APIARY_GUARDRAIL_DIR APIARY_RAFTD_JSON APIARY_MANAGERD_JSON APIARY_COMMON_JSON
}

n=0

echo "== a normal force-restart records the voter, with lease_id 0 =="
fresh
printf '{ "data_dir": "/x", "node_id": "brood" }\n' > "$APIARY_RAFTD_JSON"
sh "$here/record-forced-restart.sh" raftd >/dev/null 2>&1
check "file name uses the apiary_ prefix" \
	"yes" "$([ -f "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json" ] && echo yes || echo no)"
check "record is the exact shape both Go stores parse" \
	'{"service":"apiary_raftd","node_id":"brood","lease_id":0}' \
	"$(cat "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json")"
check "lease_id is 0, so no real lease can ever be released by it" \
	"0" \
	"$(sed -n 's/.*"lease_id":\([0-9]*\).*/\1/p' "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json")"
check "record is not world-readable" \
	"600" \
	"$(stat -f '%Lp' "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json" 2>/dev/null || stat -c '%a' "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json")"

echo
echo "== a bare service name is prefixed, so the Makefile can pass raftd =="
fresh
printf '{ "node_id": "brood" }\n' > "$APIARY_RAFTD_JSON"
sh "$here/record-forced-restart.sh" managerd >/dev/null 2>&1
check "managerd lands under its apiary_ name" \
	'{"service":"apiary_managerd","node_id":"brood","lease_id":0}' \
	"$(cat "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_managerd.json")"

echo
echo "== an existing record is never overwritten =="
fresh
printf '{ "node_id": "brood" }\n' > "$APIARY_RAFTD_JSON"
mkdir -p "$APIARY_GUARDRAIL_DIR"
printf '{"service":"apiary_raftd","node_id":"brood","lease_id":77}\n' \
	> "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json"
sh "$here/record-forced-restart.sh" raftd >/dev/null 2>&1
check "a real lease's record survives untouched" \
	'{"service":"apiary_raftd","node_id":"brood","lease_id":77}' \
	"$(cat "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json")"
check "and the operator is told the cooldown will not learn" \
	"yes" \
	"$(sh "$here/record-forced-restart.sh" raftd 2>&1 | grep -q 'will NOT learn' && echo yes || echo no)"

echo
echo "== a Comb whose configs omit node_id falls back to the hostname, like the daemons do =="
fresh
sh "$here/record-forced-restart.sh" raftd >/dev/null 2>&1
check "records under the hostname when no config file sets node_id" \
	"{\"service\":\"apiary_raftd\",\"node_id\":\"$(hostname)\",\"lease_id\":0}" \
	"$(cat "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json" 2>/dev/null)"
check "the hostname fallback does not warn about learning nothing" \
	"no" \
	"$(sh "$here/record-forced-restart.sh" managerd 2>&1 | grep -q 'will NOT learn' && echo yes || echo no)"

echo
echo "== common.json is consulted before the hostname, as ADR-0111's chain requires =="
fresh
printf '{ "node_id": "sting" }\n' > "$APIARY_COMMON_JSON"
sh "$here/record-forced-restart.sh" raftd >/dev/null 2>&1
check "falls back to common.json's node_id" \
	'{"service":"apiary_raftd","node_id":"sting","lease_id":0}' \
	"$(cat "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json")"

echo
echo "== when even the hostname is unavailable, write nothing rather than a record that never blocks =="
fresh
mkdir -p "$D/bin"
printf '#!/bin/sh\nexit 1\n' > "$D/bin/hostname"
chmod +x "$D/bin/hostname"
out=$(PATH="$D/bin:$PATH" sh "$here/record-forced-restart.sh" raftd 2>&1)
check "no record file was created" \
	"no" \
	"$([ -e "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json" ] && echo yes || echo no)"
check "it says the cooldown will not learn" \
	"yes" \
	"$(echo "$out" | grep -q 'will NOT learn' && echo yes || echo no)"
check "it still exits 0, never blocking the emergency restart" \
	"0" \
	"$(PATH="$D/bin:$PATH" sh "$here/record-forced-restart.sh" raftd >/dev/null 2>&1; echo $?)"

echo
echo "== managerd.json is the fallback when raftd.json is unreadable =="
fresh
printf '{ "node_id": "drone" }\n' > "$APIARY_MANAGERD_JSON"
sh "$here/record-forced-restart.sh" raftd >/dev/null 2>&1
check "falls back to the managerd config's node_id" \
	'{"service":"apiary_raftd","node_id":"drone","lease_id":0}' \
	"$(cat "$APIARY_GUARDRAIL_DIR/pending-restart-apiary_raftd.json")"

echo
echo "== the emergency path is never blocked by this bookkeeping =="
fresh
check "bad usage still exits 0" "0" \
	"$(sh "$here/record-forced-restart.sh" >/dev/null 2>&1; echo $?)"
check "unwritable state dir still exits 0" "0" \
	"$(APIARY_GUARDRAIL_DIR=/proc/nope sh "$here/record-forced-restart.sh" raftd >/dev/null 2>&1; echo $?)"

echo
if [ "$fails" -eq 0 ] ; then
	echo "all $cases cases passed"
	exit 0
fi
echo "$fails of $cases cases FAILED"
exit 1
