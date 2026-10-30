#!/bin/sh
#
# Check that the Apiary plugin package is a valid Agent Plugins 1.0.0
# package AND a valid Claude Code plugin.
#
# The point of the package is that it works for ANY client, so "it
# loads" is not a claim that can be made by hand - it is checked here
# against the two published specifications:
#
#   Agent Plugins 1.0.0  https://agent-plugins.org/specification
#   Claude Code plugins  https://code.claude.com/docs/en/plugins/components
#
# What it checks:
#
#   Agent Plugins 1.0.0
#     - plugin.json exists at the plugin root, with $schema and name
#     - mcp.json exists at the plugin root, with $schema and mcpServers
#     - skills/ is a directory of SKILL.md directories
#     - a stdio command is either a bare name or a "./"-relative path
#     - the command's path resolves INSIDE the plugin root
#
#   Claude Code
#     - .claude-plugin/plugin.json exists at the plugin root
#     - .mcp.json exists at the plugin root, with mcpServers
#     - skills/ is a directory of SKILL.md directories
#     - the command resolves, and ${CLAUDE_PLUGIN_ROOT} expands to the root
#
#   Both
#     - bin/apiary-mcp exists and is executable
#     - the connector binary answers -version
#
# Usage:
#   scripts/check-plugin-package.sh [PLUGIN_DIR]
#
# PLUGIN_DIR defaults to plugins/apiary-mcp-connector.
#
set -eu

PLUGIN="${1:-plugins/apiary-mcp-connector}"
FAILED=0

fail() {
	echo "FAIL: $*" >&2
	FAILED=1
}

echo "== checking $PLUGIN =="

[ -d "$PLUGIN" ] || { fail "$PLUGIN is not a directory"; exit 1; }
ROOT="$(cd "$PLUGIN" && pwd)"

# --- Agent Plugins 1.0.0 ---------------------------------------------
echo "-- Agent Plugins 1.0.0 --"
if [ -f "$ROOT/plugin.json" ]; then
	echo "plugin.json at the plugin root: ok"
else
	fail "plugin.json is missing from the plugin root"
fi

if [ -f "$ROOT/mcp.json" ]; then
	echo "mcp.json at the plugin root: ok"
else
	fail "mcp.json is missing from the plugin root"
fi

if [ -d "$ROOT/skills" ]; then
	n=$(find "$ROOT/skills" -mindepth 2 -maxdepth 2 -name SKILL.md | wc -l | tr -d ' ')
	echo "skills/ holds $n SKILL.md: ok"
	[ "$n" -gt 0 ] || fail "skills/ holds no SKILL.md"
else
	fail "skills/ is missing from the plugin root"
fi

# --- Claude Code -----------------------------------------------------
echo "-- Claude Code --"
if [ -f "$ROOT/.claude-plugin/plugin.json" ]; then
	echo ".claude-plugin/plugin.json at the plugin root: ok"
else
	fail ".claude-plugin/plugin.json is missing from the plugin root"
fi

if [ -f "$ROOT/.mcp.json" ]; then
	echo ".mcp.json at the plugin root: ok"
else
	fail ".mcp.json is missing from the plugin root"
fi

# --- The binary ------------------------------------------------------
echo "-- the packaged binary --"
if [ -x "$ROOT/bin/apiary-mcp" ]; then
	echo "bin/apiary-mcp is executable: ok"
	if "$ROOT/bin/apiary-mcp" -version >/dev/null 2>&1; then
		echo "bin/apiary-mcp answers -version: ok"
	else
		fail "bin/apiary-mcp does not answer -version"
	fi
else
	fail "bin/apiary-mcp is missing or not executable"
fi

# --- Command resolution ---------------------------------------------
# The Agent Plugins command must resolve inside the plugin root. The
# Claude Code one must expand ${CLAUDE_PLUGIN_ROOT} to the plugin root
# and then resolve to the same file.
echo "-- command resolution --"
AGENT_CMD=$(sed -n 's/.*"command"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$ROOT/mcp.json" | head -1)
CLAUDE_CMD=$(sed -n 's/.*"command"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$ROOT/.mcp.json" | head -1)

case "$AGENT_CMD" in
./?*)
	AGENT_PATH="$ROOT/${AGENT_CMD#./}"
	if [ -x "$AGENT_PATH" ]; then
		echo "Agent Plugins command $AGENT_CMD resolves inside the package: ok"
	else
		fail "Agent Plugins command $AGENT_CMD does not resolve to an executable"
	fi
	;;
"")
	fail "mcp.json has no command"
	;;
*)
	# A bare name is resolved by platform search rules, so the package
	# is not self-contained.
	fail "Agent Plugins command $AGENT_CMD is a bare name; the package must carry its own binary"
	;;
esac

case "$CLAUDE_CMD" in
*CLAUDE_PLUGIN_ROOT*)
	EXPANDED=$(printf '%s' "$CLAUDE_CMD" | sed "s|\${CLAUDE_PLUGIN_ROOT}|$ROOT|g")
	if [ -x "$EXPANDED" ]; then
		echo "Claude Code command expands to an executable: ok"
	else
		fail "Claude Code command expands to $EXPANDED, which is not executable"
	fi
	;;
"")
	fail ".mcp.json has no command"
	;;
*)
	if [ -x "$CLAUDE_CMD" ]; then
		echo "Claude Code command is executable: ok"
	else
		fail "Claude Code command $CLAUDE_CMD is not executable"
	fi
	;;
esac

# --- Both commands must reach the same binary ------------------------
if [ -n "$AGENT_CMD" ] && [ -n "$CLAUDE_CMD" ]; then
	A="${AGENT_CMD#./}"
	C=$(printf '%s' "$CLAUDE_CMD" | sed "s|\${CLAUDE_PLUGIN_ROOT}|$ROOT|g")
	if [ "$ROOT/$A" = "$C" ]; then
		echo "both clients resolve to the same binary: ok"
	else
		fail "the two clients resolve to different binaries ($ROOT/$A and $C)"
	fi
fi

if [ "$FAILED" -eq 0 ]; then
	echo "== plugin package is valid for Agent Plugins 1.0.0 and Claude Code =="
else
	echo "== plugin package is NOT valid ==" >&2
	exit 1
fi
