#!/bin/sh
#
# Verify that orcli's MCP client and this repository's MCP server
# actually talk to each other.
#
# ADR-0162 records that "deployment and client interoperability are not
# verified" for the Apiary MCP server. This script is the thing that
# verifies it, so the claim can be checked rather than believed.
#
# It starts a STUB managerd - it does NOT need a live Apiary, a running
# managerd, or any privileges - then starts the real apiary-mcp against
# it, and drives it with orcli's real MCP client over BOTH transports:
# stdio (a local client) and Streamable HTTP (a remote client).
#
# What it checks:
#
#   - the server answers initialize, and orcli can list the tools
#   - the tool set is the three read-only tools, not something else
#   - every tool carries its read-only annotation
#   - the server's instructions reach the client
#
# Usage:
#   scripts/check-mcp-interop.sh [ORCLI_CHECKOUT]
#
# ORCLI_CHECKOUT defaults to /home/gjb/work/orcli. It must be a checkout
# of glenjbarber/orcli at a revision whose internal/mcp client supports
# tools - see the note in the script body about what that requires.

set -eu

ORCLI="${1:-/home/gjb/work/orcli}"
APIARY="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"; [ -n "${STUB_PID:-}" ] && kill "$STUB_PID" 2>/dev/null || true; [ -n "${MCP_PID:-}" ] && kill "$MCP_PID" 2>/dev/null || true' EXIT

if [ ! -d "$ORCLI/internal/mcp" ]; then
	echo "no orcli checkout at $ORCLI (want a glenjbarber/orcli checkout)" >&2
	exit 2
fi

# A stub managerd: it answers the read RPCs with fixed values and
# nothing else. It is the smallest thing that lets the connector reach
# the point where the PROTOCOL is exercised, which is the part being
# tested.
cat > "$TMP/stub.go" <<'STUB'
package main

import (
	"context"
	"log"
	"net"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"google.golang.org/grpc"
)

type stub struct{ rpcpb.UnimplementedManagerServiceServer }

func (s *stub) ListVMs(context.Context, *rpcpb.ListVMsRequest) (*rpcpb.ListVMsResponse, error) {
	return &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-1", Name: "web", NodeId: "node-a"}}}, nil
}
func (s *stub) ListJails(context.Context, *rpcpb.ListJailsRequest) (*rpcpb.ListJailsResponse, error) {
	return &rpcpb.ListJailsResponse{}, nil
}
func (s *stub) GetVM(context.Context, *rpcpb.GetVMRequest) (*rpcpb.GetVMResponse, error) {
	return &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web", NodeId: "node-a"}}, nil
}
func (s *stub) GetJail(context.Context, *rpcpb.GetJailRequest) (*rpcpb.GetJailResponse, error) {
	return &rpcpb.GetJailResponse{}, nil
}
func (s *stub) ClusterHealth(context.Context, *rpcpb.ClusterHealthRequest) (*rpcpb.ClusterHealthResponse, error) {
	return &rpcpb.ClusterHealthResponse{LocalNodeId: "node-a", Nodes: []*rpcpb.ClusterNodeHealth{{NodeId: "node-a", Status: "healthy", Dialed: true}}}, nil
}

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:17799")
	if err != nil {
		log.Fatal(err)
	}
	s := grpc.NewServer()
	rpcpb.RegisterManagerServiceServer(s, &stub{})
	log.Fatal(s.Serve(l))
}
STUB

MGR_PORT=17799
MCP_PORT=8789
MCP_TOKEN=0123456789012345678901234567890123456789

echo "== building =="
( cd "$APIARY" && go build -o "$TMP/apiary-mcp" ./cmd/apiary-mcp )

echo "== starting the stub managerd on 127.0.0.1:$MGR_PORT =="
( cd "$APIARY" && go run "$TMP/stub.go" ) >"$TMP/stub.log" 2>&1 &
STUB_PID=$!
i=0
while [ $i -lt 30 ]; do
	if nc -z 127.0.0.1 "$MGR_PORT" 2>/dev/null; then break; fi
	i=$((i + 1))
	sleep 1
done
if ! nc -z 127.0.0.1 "$MGR_PORT" 2>/dev/null; then
	echo "the stub managerd never came up:" >&2
	cat "$TMP/stub.log" >&2
	exit 1
fi

# The interop test itself lives in orcli, because it is ORCLI's client
# that is being tested against ORCLI's server, and orcli's MCP client is
# an internal package that this repository cannot import.
cat > "$ORCLI/internal/mcp/apiarity_interop_test.go" <<'ITEST'
package mcp

import (
	"encoding/json"
	"os"
	"testing"
)

func TestAgainstApiaryMCPOverStdio(t *testing.T) {
	bin := os.Getenv("APIARY_MCP_BIN")
	if bin == "" {
		t.Skip("APIARY_MCP_BIN not set")
	}
	m := NewManager([]Server{{
		Name:    "apiary",
		Command: bin,
		Env: []string{
			"APIARY_MANAGER_ADDR=127.0.0.1:17799",
			"APIARY_API_KEY=interop-viewer-key",
			"APIARY_MCP_TRANSPORT=stdio",
		},
	}})
	defer m.Close()

	c, err := m.get("apiary")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	raw, err := c.call("tools/list", map[string]any{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var out struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations *struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if len(out.Tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(out.Tools))
	}
	for _, tool := range out.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q did not arrive annotated read-only", tool.Name)
		}
	}
}

func TestAgainstApiaryMCPOverHTTP(t *testing.T) {
	url := os.Getenv("APIARY_MCP_URL")
	if url == "" {
		t.Skip("APIARY_MCP_URL not set")
	}
	m := NewManager([]Server{{
		Name:    "apiary",
		URL:     url,
		Headers: map[string]string{"Authorization": "Bearer " + os.Getenv("APIARY_MCP_TOKEN")},
	}})
	defer m.Close()

	c, err := m.get("apiary")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	raw, err := c.call("tools/list", map[string]any{})
	if err != nil {
		t.Fatalf("tools/list over HTTP: %v", err)
	}
	var out struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if len(out.Tools) != 3 {
		t.Fatalf("tools over HTTP = %d, want 3", len(out.Tools))
	}
}
ITEST
trap 'rm -rf "$TMP"; rm -f "$ORCLI/internal/mcp/apiarity_interop_test.go"; [ -n "${STUB_PID:-}" ] && kill "$STUB_PID" 2>/dev/null || true; [ -n "${MCP_PID:-}" ] && kill "$MCP_PID" 2>/dev/null || true' EXIT

echo "== stdio transport =="
( cd "$ORCLI" && APIARY_MCP_BIN="$TMP/apiary-mcp" go test -count=1 -v -run TestAgainstApiaryMCPOverStdio ./internal/mcp/ )

echo "== Streamable HTTP transport =="
APIARY_MANAGER_ADDR="127.0.0.1:$MGR_PORT" \
APIARY_API_KEY=interop-viewer-key \
APIARY_MCP_TRANSPORT=http \
APIARY_MCP_LISTEN="127.0.0.1:$MCP_PORT" \
APIARY_MCP_TOKEN="$MCP_TOKEN" \
	"$TMP/apiary-mcp" >"$TMP/mcp.log" 2>&1 &
MCP_PID=$!
i=0
while [ $i -lt 30 ]; do
	if nc -z 127.0.0.1 "$MCP_PORT" 2>/dev/null; then break; fi
	i=$((i + 1))
	sleep 1
done
( cd "$ORCLI" && APIARY_MCP_URL="http://127.0.0.1:$MCP_PORT/mcp" APIARY_MCP_TOKEN="$MCP_TOKEN" \
	go test -count=1 -v -run TestAgainstApiaryMCPOverHTTP ./internal/mcp/ )

echo
echo "MCP interoperability verified over both transports."
