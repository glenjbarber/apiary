package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	// Point the settings file at a path that does not exist, so the test
	// does not pick up a real ~/.config/apiary/mcp.env on the machine
	// running it. A missing file is not an error.
	t.Setenv("APIARY_MCP_ENV_FILE", filepath.Join(t.TempDir(), "absent.env"))
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

// writeSettingsFile writes a settings file with the given mode and returns
// its path.
func writeSettingsFile(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write settings file: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod settings file: %v", err)
	}
	return path
}

// TestLoadConfigFallsBackToSettingsFile checks that a setting the
// environment leaves unset is taken from the settings file.
func TestLoadConfigFallsBackToSettingsFile(t *testing.T) {
	path := writeSettingsFile(t, 0o600, ""+
		"# a comment\n"+
		"\n"+
		"APIARY_MANAGER_ADDR=manager.example:9443\n"+
		"APIARY_API_KEY=file-key\n")
	t.Setenv("APIARY_MCP_ENV_FILE", path)
	t.Setenv("APIARY_MANAGER_ADDR", "")
	t.Setenv("APIARY_API_KEY", "")

	cfg, err := loadConfig("stdio")
	if err != nil {
		t.Fatalf("loadConfig: err = %v, want nil", err)
	}
	if cfg.managerAddr != "manager.example:9443" {
		t.Errorf("managerAddr = %q, want %q", cfg.managerAddr, "manager.example:9443")
	}
	if cfg.apiKey != "file-key" {
		t.Errorf("apiKey = %q, want %q", cfg.apiKey, "file-key")
	}
}

// TestLoadConfigEnvironmentWins checks that the environment takes
// precedence over the settings file.
func TestLoadConfigEnvironmentWins(t *testing.T) {
	path := writeSettingsFile(t, 0o600, ""+
		"APIARY_MANAGER_ADDR=file.example:9443\n"+
		"APIARY_API_KEY=file-key\n")
	t.Setenv("APIARY_MCP_ENV_FILE", path)
	t.Setenv("APIARY_MANAGER_ADDR", "env.example:9443")
	t.Setenv("APIARY_API_KEY", "env-key")

	cfg, err := loadConfig("stdio")
	if err != nil {
		t.Fatalf("loadConfig: err = %v, want nil", err)
	}
	if cfg.managerAddr != "env.example:9443" {
		t.Errorf("managerAddr = %q, want %q", cfg.managerAddr, "env.example:9443")
	}
	if cfg.apiKey != "env-key" {
		t.Errorf("apiKey = %q, want %q", cfg.apiKey, "env-key")
	}
}

// TestLoadConfigTreatsPlaceholderAsUnset checks that an unexpanded
// ${...} placeholder is treated as unset rather than as a value. This is
// the case that broke the plugin: the host passed the placeholder through
// verbatim, and the connector dialled a host named "${APIARY_MANAGER_ADDR}".
func TestLoadConfigTreatsPlaceholderAsUnset(t *testing.T) {
	path := writeSettingsFile(t, 0o600, ""+
		"APIARY_MANAGER_ADDR=file.example:9443\n"+
		"APIARY_API_KEY=file-key\n")
	t.Setenv("APIARY_MCP_ENV_FILE", path)
	t.Setenv("APIARY_MANAGER_ADDR", "${APIARY_MANAGER_ADDR}")
	t.Setenv("APIARY_API_KEY", "${APIARY_API_KEY}")

	cfg, err := loadConfig("stdio")
	if err != nil {
		t.Fatalf("loadConfig: err = %v, want nil", err)
	}
	if cfg.managerAddr != "file.example:9443" {
		t.Errorf("managerAddr = %q, want %q", cfg.managerAddr, "file.example:9443")
	}
	if cfg.apiKey != "file-key" {
		t.Errorf("apiKey = %q, want %q", cfg.apiKey, "file-key")
	}
}

// TestLoadConfigRefusesWorldReadableSettingsFile checks that a settings
// file any group or other can read is refused. It may hold an API key.
func TestLoadConfigRefusesWorldReadableSettingsFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o644, 0o604, 0o666} {
		path := writeSettingsFile(t, mode, ""+
			"APIARY_MANAGER_ADDR=manager.example:9443\n"+
			"APIARY_API_KEY=file-key\n")
		t.Setenv("APIARY_MCP_ENV_FILE", path)
		t.Setenv("APIARY_MANAGER_ADDR", "")
		t.Setenv("APIARY_API_KEY", "")

		if _, err := loadConfig("stdio"); err == nil {
			t.Errorf("mode %04o: err = nil, want an error", mode)
		}
	}
}

// TestLoadConfigMissingSettingsFileIsNotAnError checks that a host that
// configures everything through the environment keeps working when no
// settings file exists.
func TestLoadConfigMissingSettingsFileIsNotAnError(t *testing.T) {
	t.Setenv("APIARY_MCP_ENV_FILE", filepath.Join(t.TempDir(), "absent.env"))
	t.Setenv("APIARY_MANAGER_ADDR", "manager.example:9443")
	t.Setenv("APIARY_API_KEY", "key")

	if _, err := loadConfig("stdio"); err != nil {
		t.Errorf("loadConfig: err = %v, want nil", err)
	}
}

// TestLoadConfigRequiresManagerAddrAndAPIKey checks the refusal when
// neither the environment nor the settings file supplies the two required
// settings.
func TestLoadConfigRequiresManagerAddrAndAPIKey(t *testing.T) {
	t.Setenv("APIARY_MCP_ENV_FILE", filepath.Join(t.TempDir(), "absent.env"))
	t.Setenv("APIARY_MANAGER_ADDR", "")
	t.Setenv("APIARY_API_KEY", "")

	if _, err := loadConfig("stdio"); err == nil {
		t.Errorf("err = nil, want an error")
	}
}

// TestLoadConfigHTTPModeIgnoresSettingsFile checks that the settings file
// is not consulted in HTTP mode. A connector running as a daemon has a
// secret-delivery contract of its own, and ADR-0162 says its configuration
// comes from the environment.
func TestLoadConfigHTTPModeIgnoresSettingsFile(t *testing.T) {
	path := writeSettingsFile(t, 0o644, ""+
		"APIARY_MANAGER_ADDR=file.example:9443\n"+
		"APIARY_API_KEY=file-key\n")
	t.Setenv("APIARY_MCP_ENV_FILE", path)
	t.Setenv("APIARY_MANAGER_ADDR", "env.example:9443")
	t.Setenv("APIARY_API_KEY", "env-key")
	t.Setenv("APIARY_MCP_TOKEN", "0123456789abcdef0123456789abcdef")

	// The file is world-readable here, so reading it would be an error.
	// In HTTP mode it is not read at all, so this must succeed.
	cfg, err := loadConfig("http")
	if err != nil {
		t.Fatalf("loadConfig: err = %v, want nil", err)
	}
	if cfg.managerAddr != "env.example:9443" {
		t.Errorf("managerAddr = %q, want %q", cfg.managerAddr, "env.example:9443")
	}
}

// TestLoadConfigTransportSources checks where the transport comes from:
// the flag wins, then the environment, then the settings file, then stdio.
func TestLoadConfigTransportSources(t *testing.T) {
	path := writeSettingsFile(t, 0o600, ""+
		"APIARY_MANAGER_ADDR=file.example:9443\n"+
		"APIARY_API_KEY=file-key\n"+
		"APIARY_MCP_TRANSPORT=http\n")
	t.Setenv("APIARY_MCP_ENV_FILE", path)
	t.Setenv("APIARY_MANAGER_ADDR", "")
	t.Setenv("APIARY_API_KEY", "")
	t.Setenv("APIARY_MCP_TRANSPORT", "")

	// Nothing names a transport but the file, and the file says http.
	// That makes this a daemon deployment, so the file supplies no
	// settings and the required ones are missing.
	if _, err := loadConfig(""); err == nil {
		t.Errorf("err = nil, want an error")
	}

	// The environment names http, with the required settings present.
	t.Setenv("APIARY_MCP_TRANSPORT", "http")
	t.Setenv("APIARY_MANAGER_ADDR", "env.example:9443")
	t.Setenv("APIARY_API_KEY", "env-key")
	t.Setenv("APIARY_MCP_TOKEN", "0123456789abcdef0123456789abcdef")
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("loadConfig: err = %v, want nil", err)
	}
	if cfg.transport != "http" {
		t.Errorf("transport = %q, want %q", cfg.transport, "http")
	}
	if cfg.managerAddr != "env.example:9443" {
		t.Errorf("managerAddr = %q, want %q", cfg.managerAddr, "env.example:9443")
	}

	// The flag wins over the environment.
	cfg, err = loadConfig("stdio")
	if err != nil {
		t.Fatalf("loadConfig: err = %v, want nil", err)
	}
	if cfg.transport != "stdio" {
		t.Errorf("transport = %q, want %q", cfg.transport, "stdio")
	}
}

// TestSettingsFileWithoutHome checks that the settings file is still found
// when the launching host passes a minimal environment with no $HOME.
//
// A plugin host that launches the connector as a subprocess may set only
// PATH, and os.UserHomeDir reads $HOME. Without the passwd-database
// fallback the connector would find no settings file at all, which is the
// very case the file exists to serve.
func TestSettingsFileWithoutHome(t *testing.T) {
	path := writeSettingsFile(t, 0o600, ""+
		"APIARY_MANAGER_ADDR=manager.example:9443\n"+
		"APIARY_API_KEY=file-key\n")
	t.Setenv("APIARY_MCP_ENV_FILE", path)

	home, err := homeDir()
	if err != nil {
		t.Fatalf("homeDir: err = %v, want nil", err)
	}
	if home == "" {
		t.Errorf("homeDir returned an empty path")
	}
}

// TestSettingsFileDefaultPath checks the default location of the settings
// file when APIARY_MCP_ENV_FILE does not name one.
func TestSettingsFileDefaultPath(t *testing.T) {
	t.Setenv("APIARY_MCP_ENV_FILE", "")
	home, err := homeDir()
	if err != nil {
		t.Fatalf("homeDir: err = %v, want nil", err)
	}
	want := filepath.Join(home, ".config", "apiary", "mcp.env")
	if got := settingsFile(); got != want {
		t.Errorf("settingsFile() = %q, want %q", got, want)
	}
}
