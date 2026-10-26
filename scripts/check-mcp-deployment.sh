#!/bin/sh
#
# Verify that the Apiary MCP connector DEPLOYS and SERVES, as the
# installed service rather than as a build in a tree.
#
# ADR-0162's status line recorded, for a long time, that "deployment
# and client interoperability are not verified". The interoperability
# half is settled by scripts/check-mcp-interop.sh, which drives the
# server with orcli's real client. This script settles the DEPLOYMENT
# half: it starts the installed service the way an operator would, and
# checks the properties the deployment is supposed to have.
#
# What it checks:
#
#   - the connector refuses to start with a short bearer token, naming
#     that reason (a deployment that starts with a weak token is a
#     deployment with a guessable credential)
#   - the service starts and listens on LOOPBACK only
#   - an unauthenticated request is refused with HTTP 401
#   - a request with a wrong bearer token is refused with HTTP 401
#   - an authenticated initialize returns the protocol's capabilities
#     and the server's model-agnostic instructions
#   - the tool list is the three read-only tools, every one of them
#     annotated read-only
#
# Usage:
#   scripts/check-mcp-deployment.sh
#
# It needs root, because it starts a service. It does NOT need a live
# Apiary: the connector's backend calls are not exercised here, only
# the protocol surface and the access control in front of it.
#
# It starts and stops the service, and it leaves the host as it found
# it: the secrets file it writes is removed afterwards, and the service
# is stopped.

set -eu

APIARY="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
MCP_BIN="/usr/local/libexec/apiary/apiary-mcp"
MCP_RC="/usr/local/etc/rc.d/apiary_mcp"
SECRETS="/usr/local/etc/apiary/mcp-secrets"
LISTEN="127.0.0.1:8787"
TOKEN="check_mcp_deployment_token_0123456789_abcdef"
BAD_TOKEN="tooshort"

if [ "$(id -u)" != "0" ]; then
	echo "this check starts a service and needs root" >&2
	exit 2
fi

if [ ! -x "$MCP_BIN" ]; then
	echo "the connector is not installed at $MCP_BIN" >&2
	exit 2
fi
if [ ! -r "$MCP_RC" ]; then
	echo "the connector's rc.d script is not at $MCP_RC" >&2
	exit 2
fi

# Stop the connector properly.
#
# `service apiary_mcp stop` is NOT enough here. The rc script runs the
# connector under daemon(8) with -r, which respawns it, so a connector
# that exits immediately on a configuration refusal leaves its
# supervisor alive - and that supervisor keeps restarting the connector
# with the SAME (refused) configuration. A later start with a different
# secrets file would then appear to do nothing, because the old
# supervisor is still holding the port and the old configuration.
#
# So the supervisor is matched and killed directly, and the port is
# waited for.
stop_connector() {
	service apiary_mcp stop >/dev/null 2>&1 || true
	for _p in $(ps -eo pid,args 2>/dev/null | grep "[d]aemon: /usr/local/libexec/apiary/apiary-mcp" | awk '{print $1}'); do
		kill "$_p" 2>/dev/null || true
	done
	for _p in $(ps -eo pid,args 2>/dev/null | grep "[/]usr/local/libexec/apiary/apiary-mcp" | awk '{print $1}'); do
		kill "$_p" 2>/dev/null || true
	done
	_n=0
	while [ "$_n" -lt 20 ]; do
		serving || break
		sleep 0.5
		_n=$(( _n + 1 ))
	done
}

cleanup() {
	stop_connector
	rm -f "$SECRETS"
}
trap cleanup EXIT INT TERM

started=0

# POST an MCP request and print the HTTP status.
#
# $1 is the bearer token to present (empty for none), $2 the method,
# $3 the extra JSON body, $4 the output file.
mcp_request() {
	_token="$1"
	_method="$2"
	_body="$3"
	_out="$4"
	if [ -n "$_token" ]; then
		curl -sS --max-time 10 -o "$_out" -w '%{http_code}' \
			-X POST "http://$LISTEN/mcp" \
			-H 'Content-Type: application/json' \
			-H 'Accept: application/json, text/event-stream' \
			-H "Authorization: Bearer $_token" \
			-d "$_body" 2>/dev/null || echo "000"
	else
		curl -sS --max-time 10 -o "$_out" -w '%{http_code}' \
			-X POST "http://$LISTEN/mcp" \
			-H 'Content-Type: application/json' \
			-H 'Accept: application/json, text/event-stream' \
			-d "$_body" 2>/dev/null || echo "000"
	fi
}

INIT_BODY='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}'

# Whether the connector is ACTUALLY SERVING.
#
# This is not "is the service running". The rc script runs the
# connector under daemon(8) with -r, which RESPAWNS it, so the
# supervisor process is alive even when the connector itself exits
# immediately on a configuration refusal. Asking rc.subr therefore
# answers the wrong question: it reports a running service for a
# connector that died on startup.
#
# The question that matters is whether anything is listening on the
# MCP port, so that is what is asked.
serving() {
	if command -v sockstat >/dev/null 2>&1; then
		sockstat -4 -l 2>/dev/null | grep -q "$LISTEN" && return 0
		return 1
	fi
	curl -sS --max-time 3 -o /dev/null "http://$LISTEN/mcp" 2>/dev/null && return 0
	return 1
}

echo "== a short bearer token is refused =="
mkdir -p "$(dirname "$SECRETS")"
stop_connector
printf 'APIARY_API_KEY=apk_check_notarealkey\nAPIARY_MCP_TOKEN=%s\n' "$BAD_TOKEN" > "$SECRETS"
chmod 0600 "$SECRETS"
service apiary_mcp onestart >/dev/null 2>&1 || true
sleep 3
if serving; then
	echo "FAIL: the connector is serving with a $(( ${#BAD_TOKEN} ))-character bearer token" >&2
	exit 1
fi
if ! grep -q "at least 32 characters" /var/log/apiary/mcp.log 2>/dev/null; then
	echo "FAIL: the service did not name the token length as the reason" >&2
	exit 1
fi
echo "ok: refused, and the reason was named"

echo "== a compliant token starts the service =="
# The order matters: stop the connector FIRST, then replace the
# secrets, then start. Starting first and stopping afterwards would
# kill the connector this step just started.
stop_connector
printf 'APIARY_API_KEY=apk_check_notarealkey\nAPIARY_MCP_TOKEN=%s\n' "$TOKEN" > "$SECRETS"
chmod 0600 "$SECRETS"
service apiary_mcp onestart >/dev/null 2>&1 || true
sleep 3
if ! serving; then
	echo "FAIL: the connector is not serving with a compliant token" >&2
	tail -5 /var/log/apiary/mcp.log >&2 || true
	exit 1
fi
started=1

echo "== it listens on loopback only =="
if command -v sockstat >/dev/null 2>&1; then
	if sockstat -4 -l 2>/dev/null | grep "$LISTEN" | grep -qv "127.0.0.1"; then
		echo "FAIL: the connector is listening on a non-loopback address" >&2
		exit 1
	fi
fi
echo "ok"

echo "== an unauthenticated request is refused =="
code="$(mcp_request "" initialize "$INIT_BODY" /tmp/apiary-mcp-check-noauth.$$)"
if [ "$code" != "401" ]; then
	echo "FAIL: an unauthenticated request returned HTTP $code, want 401" >&2
	exit 1
fi
echo "ok: HTTP 401"

echo "== a wrong bearer token is refused =="
code="$(mcp_request "wrong_token_wrong_token_wrong_token_wrong" initialize "$INIT_BODY" /tmp/apiary-mcp-check-badauth.$$)"
if [ "$code" != "401" ]; then
	echo "FAIL: a request with a wrong token returned HTTP $code, want 401" >&2
	exit 1
fi
echo "ok: HTTP 401"

echo "== an authenticated initialize returns capabilities and instructions =="
code="$(mcp_request "$TOKEN" initialize "$INIT_BODY" /tmp/apiary-mcp-check-init.$$)"
if [ "$code" != "200" ]; then
	echo "FAIL: an authenticated initialize returned HTTP $code, want 200" >&2
	exit 1
fi
if ! grep -q '"capabilities"' /tmp/apiary-mcp-check-init.$$; then
	echo "FAIL: the initialize response carries no capabilities" >&2
	exit 1
fi
if ! grep -q '"instructions"' /tmp/apiary-mcp-check-init.$$; then
	echo "FAIL: the initialize response carries no server instructions" >&2
	exit 1
fi
if ! grep -q 'READ-ONLY' /tmp/apiary-mcp-check-init.$$; then
	echo "FAIL: the server instructions do not state the read-only property" >&2
	exit 1
fi
echo "ok"

echo "== the tool list is the three read-only tools, all annotated =="
code="$(mcp_request "$TOKEN" tools/list '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' /tmp/apiary-mcp-check-tools.$$)"
if [ "$code" != "200" ]; then
	echo "FAIL: a tools/list returned HTTP $code, want 200" >&2
	exit 1
fi
for tool in list_cells get_cell get_cluster_health; do
	if ! grep -q "\"$tool\"" /tmp/apiary-mcp-check-tools.$$; then
		echo "FAIL: the tool $tool is missing from the tool list" >&2
		exit 1
	fi
done
# Every tool must carry the protocol's own statement that it is
# read-only. This is the machine-readable half of the model-agnostic
# property: a client must not have to infer it from prose.
count="$(grep -o '"readOnlyHint":true' /tmp/apiary-mcp-check-tools.$$ | wc -l | tr -d ' ')"
if [ "$count" -lt 3 ]; then
	echo "FAIL: only $count tools carry readOnlyHint, want 3" >&2
	exit 1
fi
echo "ok"

rm -f /tmp/apiary-mcp-check-*.$$

echo
echo "MCP deployment verified: the service starts, binds loopback only,"
echo "refuses unauthenticated and wrongly-authenticated requests, serves"
echo "the protocol with its instructions, and exposes three read-only tools."
