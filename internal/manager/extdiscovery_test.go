package manager

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/extdiscovery"
	"github.com/glenjbarber/apiary/internal/proxmoxdiscovery"
	"github.com/glenjbarber/apiary/internal/sylvediscovery"
)

// newDiscoveryTestProvider builds a Proxmox provider pointed at a test
// server, with a CA bundle the client trusts.
func newDiscoveryTestProvider(t *testing.T, handler http.Handler) *proxmoxdiscovery.Provider {
	t.Helper()

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pemOf(srv.Certificate().Raw), 0o600); err != nil {
		t.Fatalf("writing CA bundle: %v", err)
	}
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("t"), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}

	p, err := proxmoxdiscovery.New(proxmoxdiscovery.Config{
		BaseURL:   srv.URL,
		CAFile:    caPath,
		TokenFile: tokenPath,
	})
	if err != nil {
		t.Fatalf("proxmoxdiscovery.New: %v", err)
	}
	return p
}

// A node with NO discovery configured must report an empty result, not
// an error. That is a normal configuration.
func TestGetExternalDiscovery_NoProvidersConfigured(t *testing.T) {
	srv := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := srv.GetExternalDiscovery(context.Background(), &rpcpb.GetExternalDiscoveryRequest{})
	if err != nil {
		t.Fatalf("GetExternalDiscovery: %v", err)
	}
	if len(resp.GetProviders()) != 0 {
		t.Errorf("providers = %d, want 0", len(resp.GetProviders()))
	}
}

// A provider that is configured and working must appear in the report
// with its observations.
func TestGetExternalDiscovery_ReportsProviderObservations(t *testing.T) {
	provider := newDiscoveryTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cluster/status":
			json.NewEncoder(w).Encode([]map[string]any{
				{"type": "cluster", "id": "cluster", "name": "prod", "nodes": 1},
			})
		case "/nodes":
			json.NewEncoder(w).Encode([]map[string]any{
				{"node": "p1", "type": "node", "status": "online", "ssl_fingerprint": "FP1"},
			})
		case "/nodes/p1/status":
			json.NewEncoder(w).Encode(map[string]any{"boot-info": map[string]any{"mode": "efi"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	state := &discoveryState{providers: map[string]*discoveryProvider{
		proxmoxdiscovery.ProviderName: {provider: provider, store: extdiscovery.NewStore(), interval: time.Minute},
	}}
	state.providers[proxmoxdiscovery.ProviderName].store.Apply(provider.Scan(context.Background()))

	srv := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	srv.discovery = state

	resp, err := srv.GetExternalDiscovery(context.Background(), &rpcpb.GetExternalDiscoveryRequest{})
	if err != nil {
		t.Fatalf("GetExternalDiscovery: %v", err)
	}
	if len(resp.GetProviders()) != 1 {
		t.Fatalf("providers = %d, want 1", len(resp.GetProviders()))
	}
	report := resp.GetProviders()[0]
	if report.GetProvider() != "proxmox" {
		t.Errorf("provider = %q, want proxmox", report.GetProvider())
	}
	if report.GetState() != string(extdiscovery.StateFresh) {
		t.Errorf("state = %q, want fresh", report.GetState())
	}
	if report.GetSourceId() != "prod" {
		t.Errorf("source id = %q, want prod", report.GetSourceId())
	}
	if len(report.GetNodes()) != 1 {
		t.Fatalf("nodes = %d, want 1", len(report.GetNodes()))
	}
	if report.GetNodes()[0].GetFingerprint() != "FP1" {
		t.Errorf("fingerprint = %q, want FP1", report.GetNodes()[0].GetFingerprint())
	}
}

// A provider name that is NOT configured must be reported as
// unavailable with a reason - not silently omitted, because "that
// provider does not exist here" and "that provider found nothing" are
// different answers.
func TestGetExternalDiscovery_UnknownProviderIsReported(t *testing.T) {
	srv := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	srv.discovery = &discoveryState{providers: map[string]*discoveryProvider{}}

	resp, err := srv.GetExternalDiscovery(context.Background(), &rpcpb.GetExternalDiscoveryRequest{Provider: "nonesuch"})
	if err != nil {
		t.Fatalf("GetExternalDiscovery: %v", err)
	}
	if len(resp.GetProviders()) != 1 {
		t.Fatalf("providers = %d, want 1", len(resp.GetProviders()))
	}
	report := resp.GetProviders()[0]
	if report.GetState() != string(extdiscovery.StateUnavailable) {
		t.Errorf("state = %q, want unavailable", report.GetState())
	}
	if report.GetError() == "" {
		t.Error("error is empty; an unknown provider must say so")
	}
}

// A provider that is configured but CANNOT be constructed must not
// stop the others from working, and must not appear as a working
// provider.
func TestNewDiscoveryState_OneBrokenProviderDoesNotBreakTheOther(t *testing.T) {
	// A plaintext URL is refused at construction.
	broken := &proxmoxdiscovery.Config{BaseURL: "http://insecure.example.com", TokenFile: "/tmp/x"}

	sylveSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cluster":
			w.Write([]byte(`{"status":"success","data":{"partial":false}}`))
		case "/api/cluster/nodes":
			w.Write([]byte(`{"status":"success","data":[]}`))
		case "/api/cluster/resources":
			w.Write([]byte(`{"status":"success","data":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer sylveSrv.Close()

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(caPath, pemOf(sylveSrv.Certificate().Raw), 0o600)
	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("t"), 0o600)

	good := &sylvediscovery.Config{
		BaseURL:   sylveSrv.URL + "/api",
		CAFile:    caPath,
		TokenFile: tokenPath,
	}

	state := newDiscoveryState(broken, good, 0, 0)
	if _, ok := state.providers[proxmoxdiscovery.ProviderName]; ok {
		t.Error("the broken Proxmox provider was registered; a provider that cannot be constructed must be skipped")
	}
	if _, ok := state.providers[sylvediscovery.ProviderName]; !ok {
		t.Error("the working Sylve provider was not registered; one broken provider must not disable another")
	}
}

// The discovery RPC must not be reachable without the right role. The
// role map is checked by TestRequiredRole_CoversEveryRPC; this checks
// the entry is Viewer, so a read of another system's state cannot be
// gated behind Admin by accident.
func TestExternalDiscoveryIsViewerTier(t *testing.T) {
	if got := requiredRoleFor("/apiary.rpc.v1.ManagerService/GetExternalDiscovery"); got != RoleViewer {
		t.Errorf("role = %v, want Viewer", got)
	}
}

// pemOf renders DER bytes as a PEM certificate.
func pemOf(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
