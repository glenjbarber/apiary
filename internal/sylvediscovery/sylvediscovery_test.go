package sylvediscovery

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/extdiscovery"
)

// newTestProvider builds a provider pointed at a test server.
func newTestProvider(t *testing.T, handler http.Handler) *Provider {
	t.Helper()

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		t.Fatalf("writing CA bundle: %v", err)
	}

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}

	p, err := New(Config{
		BaseURL:   srv.URL + "/api",
		CAFile:    caPath,
		TokenFile: tokenPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// sylveEnvelope renders Sylve's APIResponse wrapper.
func sylveEnvelope(t *testing.T, w http.ResponseWriter, status, message string, data any) {
	t.Helper()
	body := map[string]any{"status": status, "message": message, "error": ""}
	if status == "error" {
		body["error"] = message
		body["data"] = nil
	} else {
		body["data"] = data
	}
	json.NewEncoder(w).Encode(body)
}

// A successful clustered scan maps nodes and guests.
func TestScan_ClusteredSylve(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cluster":
			sylveEnvelope(t, w, "success", "cluster_fetched", map[string]any{
				"clusterId": "sylve-cluster",
				"enabled":   true,
				"nodeId":    "node-a",
				"partial":   false,
			})
		case "/api/cluster/nodes":
			sylveEnvelope(t, w, "success", "nodes_listed", []map[string]any{
				{"uuid": "UUID-A", "hostname": "sylve-a", "status": "online"},
				{"uuid": "UUID-B", "hostname": "sylve-b", "status": "offline"},
			})
		case "/api/cluster/resources":
			sylveEnvelope(t, w, "success", "resources_listed", []map[string]any{
				{
					"hostname": "sylve-a",
					"nodeUUID": "UUID-A",
					"vms": []map[string]any{
						{"id": "vm-1", "rid": 100, "name": "web-01", "state": "running"},
					},
					"jails": []map[string]any{
						{"id": "jail-1", "ctid": 5, "name": "dns-01", "state": "running"},
					},
				},
			})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	result := p.Scan(context.Background())

	if result.State != extdiscovery.StateFresh {
		t.Fatalf("state = %q, want %q (err: %v)", result.State, extdiscovery.StateFresh, result.Err)
	}
	if !result.Complete {
		t.Error("complete = false, want true")
	}
	if result.SourceID != "sylve-cluster" {
		t.Errorf("source id = %q, want sylve-cluster", result.SourceID)
	}
	if len(result.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(result.Nodes))
	}
	if result.Nodes[0].Fingerprint != "UUID-A" {
		t.Errorf("fingerprint = %q, want UUID-A", result.Nodes[0].Fingerprint)
	}
	if len(result.Resources) != 2 {
		t.Fatalf("resources = %d, want 2", len(result.Resources))
	}

	// The VM and the jail must remain DISTINCT kinds even though
	// Apiary models both as cells.
	var vm, jail *extdiscovery.Resource
	for i := range result.Resources {
		switch result.Resources[i].Kind {
		case extdiscovery.KindVM:
			vm = &result.Resources[i]
		case extdiscovery.KindJail:
			jail = &result.Resources[i]
		}
	}
	if vm == nil || vm.SourceResourceID != "100" || vm.Name != "web-01" {
		t.Errorf("vm = %+v, want rid 100 named web-01", vm)
	}
	if jail == nil || jail.SourceResourceID != "5" || jail.Name != "dns-01" {
		t.Errorf("jail = %+v, want ctid 5 named dns-01", jail)
	}
}

// The ENVELOPE is the trap. An error response has a null data field,
// and a client that reads data without checking status would report it
// as an empty inventory.
func TestScan_ErrorEnvelopeIsNotAnEmptyInventory(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The exact shape Sylve returns on failure.
		sylveEnvelope(t, w, "error", "list_nodes_failed", nil)
	}))

	result := p.Scan(context.Background())

	if result.State == extdiscovery.StateEmpty {
		t.Fatalf("state = %q; an error envelope must not be read as an empty inventory", result.State)
	}
	if result.Complete {
		t.Error("complete = true after an error envelope")
	}
	if result.Err == nil {
		t.Error("err = nil after an error envelope")
	}
}

// A PARTIAL cluster response is not a complete scan, whatever else it
// contained.
func TestScan_PartialClusterResponseIsPartial(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cluster":
			sylveEnvelope(t, w, "success", "cluster_fetched", map[string]any{
				"clusterId": "c", "enabled": true, "partial": true,
			})
		case "/api/cluster/nodes":
			sylveEnvelope(t, w, "success", "nodes_listed", []map[string]any{
				{"uuid": "UUID-A", "hostname": "sylve-a", "status": "online"},
			})
		case "/api/cluster/resources":
			sylveEnvelope(t, w, "success", "resources_listed", []map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	result := p.Scan(context.Background())

	if result.State != extdiscovery.StatePartial {
		t.Errorf("state = %q, want %q", result.State, extdiscovery.StatePartial)
	}
	if result.Complete {
		t.Error("complete = true after a partial response")
	}
}

// A 401 is unauthorized, NOT empty. A 403 is forbidden - a
// configuration blocker, not an inventory problem.
func TestScan_AuthFailuresAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   extdiscovery.State
	}{
		{http.StatusUnauthorized, extdiscovery.StateUnauthorized},
		{http.StatusForbidden, extdiscovery.StateForbidden},
	} {
		p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		result := p.Scan(context.Background())
		if result.State != tc.want {
			t.Errorf("HTTP %d: state = %q, want %q", tc.status, result.State, tc.want)
		}
		if result.Complete {
			t.Errorf("HTTP %d: complete = true, want false", tc.status)
		}
	}
}

// A standalone Sylve installation has no cluster id. The empty source
// id must be handled, not invented away.
func TestScan_StandaloneSylveHasNoClusterID(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cluster":
			sylveEnvelope(t, w, "success", "cluster_fetched", map[string]any{
				"enabled": false, "nodeId": "solo", "partial": false,
			})
		case "/api/cluster/nodes":
			sylveEnvelope(t, w, "success", "nodes_listed", []map[string]any{
				{"uuid": "UUID-S", "hostname": "sylve-solo", "status": "online"},
			})
		case "/api/cluster/resources":
			sylveEnvelope(t, w, "success", "resources_listed", []map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	result := p.Scan(context.Background())

	if result.SourceID != "" {
		t.Errorf("source id = %q, want empty for a standalone Sylve", result.SourceID)
	}
	if result.State != extdiscovery.StateFresh {
		t.Errorf("state = %q, want %q", result.State, extdiscovery.StateFresh)
	}
}

// A successful scan that found nothing is EMPTY - a fact.
func TestScan_EmptyIsAFact(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cluster":
			sylveEnvelope(t, w, "success", "cluster_fetched", map[string]any{"partial": false})
		case "/api/cluster/nodes":
			sylveEnvelope(t, w, "success", "nodes_listed", []map[string]any{})
		case "/api/cluster/resources":
			sylveEnvelope(t, w, "success", "resources_listed", []map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	result := p.Scan(context.Background())

	if result.State != extdiscovery.StateEmpty {
		t.Errorf("state = %q, want %q", result.State, extdiscovery.StateEmpty)
	}
	if !result.Complete {
		t.Error("complete = false, want true")
	}
}

// The provider must never request a path outside its allowlist, and
// must NOT reach the SSE endpoint - its token travels in a query
// string and would land in access logs.
func TestAllowedEndpoints_ExcludeSSEAndMutations(t *testing.T) {
	for _, forbidden := range []string{
		"/events/stream", "/cluster/join", "/cluster/leave", "/cluster/reset-node",
		"/vm/create", "/vm/{rid}/start", "/vm/{rid}/stop", "/jail/create",
		"/jail/{ctid}/start", "/jail/{ctid}/stop",
	} {
		for _, allowed := range allowedEndpoints {
			if allowed == forbidden {
				t.Errorf("path %q is in the allowlist", forbidden)
			}
		}
	}
}

// The provider must only ever issue GET.
func TestScan_OnlyIssuesGET(t *testing.T) {
	var methods []string
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		switch r.URL.Path {
		case "/api/cluster":
			sylveEnvelope(t, w, "success", "ok", map[string]any{"partial": false})
		case "/api/cluster/nodes":
			sylveEnvelope(t, w, "success", "ok", []map[string]any{})
		case "/api/cluster/resources":
			sylveEnvelope(t, w, "success", "ok", []map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	p.Scan(context.Background())

	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("issued %s; a read-only provider must only issue GET", m)
		}
	}
}

// A non-HTTPS base URL must be refused at construction.
func TestNew_RefusesPlaintextHTTP(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://sylve.example.com:8181/api", TokenFile: "/tmp/x"}); err == nil {
		t.Fatal("New accepted an http:// base URL")
	}
}

// A malformed body must be a failure, not an empty inventory.
func TestScan_MalformedBodyFails(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not json"))
	}))

	result := p.Scan(context.Background())

	if result.Complete {
		t.Error("complete = true after a malformed body")
	}
	if result.State == extdiscovery.StateEmpty {
		t.Error("state = empty after a malformed body")
	}
}

// The certificate must be verified. A server presenting a certificate
// signed by an untrusted CA must be refused.
func TestScan_RefusesUntrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sylveEnvelope(t, w, "success", "ok", map[string]any{"partial": false})
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("t"), 0o600)

	// No CAFile: the system trust store will not know this
	// certificate.
	p, err := New(Config{BaseURL: srv.URL + "/api", TokenFile: tokenPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result := p.Scan(context.Background())
	if result.State == extdiscovery.StateFresh {
		t.Error("scan succeeded against an untrusted certificate; TLS verification must be mandatory")
	}
}

// A configured CA bundle makes the same server trusted, proving the
// private-CA path works.
func TestScan_TrustedCABundleIsAccepted(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cluster":
			sylveEnvelope(t, w, "success", "ok", map[string]any{"partial": false})
		case "/api/cluster/nodes":
			sylveEnvelope(t, w, "success", "ok", []map[string]any{})
		case "/api/cluster/resources":
			sylveEnvelope(t, w, "success", "ok", []map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)

	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("t"), 0o600)

	p, err := New(Config{BaseURL: srv.URL + "/api", CAFile: caPath, TokenFile: tokenPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result := p.Scan(context.Background())
	// The fixture returns no nodes and no resources, so a successful
	// scan is EMPTY - which is the point: TLS verification passed and
	// the scan completed, rather than failing on the certificate.
	if result.State != extdiscovery.StateEmpty {
		t.Errorf("state = %q, want %q (err: %v)", result.State, extdiscovery.StateEmpty, result.Err)
	}
	if !result.Complete {
		t.Error("complete = false; the trusted-CA scan should have completed")
	}
}

// The credential must never appear in an error message.
func TestScan_CredentialIsNotLeakedInErrors(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status":"error","message":"invalid token test-token"}`))
	}))

	result := p.Scan(context.Background())

	if result.Err == nil {
		t.Fatal("err = nil")
	}
	if strings.Contains(result.Err.Error(), "test-token") {
		t.Errorf("error message leaked the credential: %v", result.Err)
	}
}

// A CA bundle that is not valid PEM must be rejected at construction,
// not silently ignored.
func TestNew_RejectsInvalidCABundle(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "bad.pem")
	os.WriteFile(caPath, []byte("not a certificate"), 0o600)

	_, err := New(Config{BaseURL: "https://example.com/api", CAFile: caPath, TokenFile: "/tmp/x"})
	if err == nil {
		t.Fatal("New accepted an invalid CA bundle")
	}
}

// The x509 import is used by the CA-bundle assertions above; this
// keeps the import honest if the tests are edited.
var _ = x509.NewCertPool
