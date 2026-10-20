package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeManager implements only the five read RPCs the connector is allowed to
// call. Any other method is reached through the nil embedded interface and
// panics, so a stray call fails loudly instead of passing silently.
type fakeManager struct {
	rpcpb.ManagerServiceClient

	mu      sync.Mutex
	called  []string
	vmErr   error
	vms     []*rpcpb.VMDefinition
	jails   []*rpcpb.JailDefinition
	vmFound bool
	health  *rpcpb.ClusterHealthResponse
}

func (f *fakeManager) record(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = append(f.called, method)
}

func (f *fakeManager) ListVMs(_ context.Context, _ *rpcpb.ListVMsRequest, _ ...grpc.CallOption) (*rpcpb.ListVMsResponse, error) {
	f.record("ListVMs")
	if f.vmErr != nil {
		return nil, f.vmErr
	}
	return &rpcpb.ListVMsResponse{Vms: f.vms}, nil
}

func (f *fakeManager) ListJails(_ context.Context, _ *rpcpb.ListJailsRequest, _ ...grpc.CallOption) (*rpcpb.ListJailsResponse, error) {
	f.record("ListJails")
	return &rpcpb.ListJailsResponse{Jails: f.jails}, nil
}

func (f *fakeManager) GetVM(_ context.Context, req *rpcpb.GetVMRequest, _ ...grpc.CallOption) (*rpcpb.GetVMResponse, error) {
	f.record("GetVM")
	if !f.vmFound {
		return &rpcpb.GetVMResponse{Found: false}, nil
	}
	return &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: req.GetId(), Name: "vm-" + req.GetId()}}, nil
}

func (f *fakeManager) GetJail(_ context.Context, req *rpcpb.GetJailRequest, _ ...grpc.CallOption) (*rpcpb.GetJailResponse, error) {
	f.record("GetJail")
	return &rpcpb.GetJailResponse{Found: false}, nil
}

func (f *fakeManager) ClusterHealth(_ context.Context, _ *rpcpb.ClusterHealthRequest, _ ...grpc.CallOption) (*rpcpb.ClusterHealthResponse, error) {
	f.record("ClusterHealth")
	if f.health != nil {
		return f.health, nil
	}
	return &rpcpb.ClusterHealthResponse{}, nil
}

// connect runs the real MCP server against an in-memory client session, so
// tests exercise the same protocol path an AI client uses.
func connect(t *testing.T, fake *fakeManager) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := newMCPServer(config{client: fake, apiKey: "test-key"}).Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) protocol error: %v", name, err)
	}
	return res
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatalf("result has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

func TestToolSurfaceIsReadOnlyAndCallsOnlyReadRPCs(t *testing.T) {
	fake := &fakeManager{
		vms:   []*rpcpb.VMDefinition{{Id: "vm-1", Name: "web"}},
		jails: []*rpcpb.JailDefinition{{Id: "jail-1", Name: "cache"}},
		health: &rpcpb.ClusterHealthResponse{
			LocalNodeId: "node-a",
			Nodes:       []*rpcpb.ClusterNodeHealth{{NodeId: "node-a", Status: "healthy", Dialed: true}},
		},
	}
	s := connect(t, fake)

	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"get_cell", "get_cluster_health", "list_cells"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	call(t, s, "list_cells", map[string]any{})
	call(t, s, "get_cell", map[string]any{"id": "vm-1", "type": "vm"})
	call(t, s, "get_cluster_health", map[string]any{})

	for _, m := range fake.called {
		if !slices.Contains(readMethods, m) {
			t.Errorf("connector called %s, which is not a read method", m)
		}
	}
}

func TestListCellsTruncatesAt200(t *testing.T) {
	fake := &fakeManager{}
	for i := 0; i < 150; i++ {
		fake.vms = append(fake.vms, &rpcpb.VMDefinition{Id: fmt.Sprintf("vm-%03d", i)})
	}
	for i := 0; i < 100; i++ {
		fake.jails = append(fake.jails, &rpcpb.JailDefinition{Id: fmt.Sprintf("jail-%03d", i)})
	}
	var out listCellsOutput
	res := call(t, connect(t, fake), "list_cells", map[string]any{})
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Cells) != 200 || !out.Truncated {
		t.Fatalf("cells = %d truncated = %v, want 200 and true", len(out.Cells), out.Truncated)
	}
}

// A Viewer-tier key that gets denied must be told what it lacks. The backend's
// own wording must not reach the client.
func TestPermissionDeniedNamesTheViewerRole(t *testing.T) {
	fake := &fakeManager{vmErr: status.Error(codes.PermissionDenied, "role operator required for /apiary.rpc.v1.ManagerService/ListVMs")}
	res := call(t, connect(t, fake), "list_cells", map[string]any{})
	if !res.IsError {
		t.Fatalf("IsError = false, want a tool error on denial")
	}
	msg := text(t, res)
	if !strings.Contains(msg, "Viewer role") {
		t.Errorf("message = %q, want it to name the Viewer role", msg)
	}
	if strings.Contains(msg, "operator required") {
		t.Errorf("message = %q, leaked the backend's own wording", msg)
	}
}

func TestBackendDetailIsNotEchoed(t *testing.T) {
	fake := &fakeManager{vmErr: status.Error(codes.Internal, "open /var/db/apiary/secret.key: permission denied")}
	res := call(t, connect(t, fake), "list_cells", map[string]any{})
	if !res.IsError {
		t.Fatalf("IsError = false, want a tool error")
	}
	msg := text(t, res)
	if strings.Contains(msg, "secret.key") || strings.Contains(msg, "/var/db") {
		t.Errorf("message = %q, leaked backend detail", msg)
	}
	if !strings.Contains(msg, "Internal") {
		t.Errorf("message = %q, want the status code named", msg)
	}
}

func TestGetCellRejectsUnknownType(t *testing.T) {
	res := call(t, connect(t, &fakeManager{}), "get_cell", map[string]any{"id": "x", "type": "switch"})
	if !res.IsError {
		t.Fatalf("IsError = false, want a validation error for type switch")
	}
}

func TestGetCellReportsNotFound(t *testing.T) {
	res := call(t, connect(t, &fakeManager{vmFound: false}), "get_cell", map[string]any{"id": "vm-9", "type": "vm"})
	if !res.IsError || !strings.Contains(text(t, res), "not found") {
		t.Fatalf("want a not-found tool error, got %q", text(t, res))
	}
}

func TestBearerGuardRejectsWrongToken(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	reached := false
	guard := bearerGuard(token, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	for name, header := range map[string]string{
		"missing": "",
		"wrong":   "Bearer nope",
		"basic":   "Basic " + token,
	} {
		reached = false
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || reached {
			t.Errorf("%s: status = %d reached = %v, want 401 and not reached", name, rec.Code, reached)
		}
	}

	reached = false
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	guard.ServeHTTP(httptest.NewRecorder(), req)
	if !reached {
		t.Errorf("correct token did not reach the handler")
	}
}

func TestLoadConfigRules(t *testing.T) {
	t.Setenv("APIARY_MANAGER_ADDR", "manager.example:9443")
	t.Setenv("APIARY_API_KEY", "key")
	t.Setenv("APIARY_MCP_TOKEN", "short")
	if _, err := loadConfig("http"); err == nil {
		t.Errorf("http with a short token: err = nil, want an error")
	}

	t.Setenv("APIARY_MCP_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("APIARY_MCP_LISTEN", "0.0.0.0:8787")
	if _, err := loadConfig("http"); err == nil {
		t.Errorf("http on a non-loopback address: err = nil, want an error")
	}

	t.Setenv("APIARY_MCP_LISTEN", "127.0.0.1:8787")
	if _, err := loadConfig("stdio"); err != nil {
		t.Errorf("stdio: err = %v, want nil", err)
	}
	if _, err := loadConfig("bogus"); err == nil {
		t.Errorf("unknown transport: err = nil, want an error")
	}
}
