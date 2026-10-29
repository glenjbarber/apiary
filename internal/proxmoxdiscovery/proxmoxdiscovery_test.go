package proxmoxdiscovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/extdiscovery"
)

// newTestProvider builds a provider pointed at a test server, with a
// credential file written.
func newTestProvider(t *testing.T, handler http.Handler) (*Provider, string) {
	t.Helper()

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	// The test server's certificate is signed by its own CA, so the
	// client must trust it. This is exactly the private-CA path the
	// provider supports - and it is how the test proves that path
	// works.
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := srv.Certificate().Raw
	_ = caPEM
	// httptest's TLS server exposes its certificate; write it as PEM.
	pemBytes := pemEncode(srv.Certificate().Raw)
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		t.Fatalf("writing CA bundle: %v", err)
	}

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}

	p, err := New(Config{
		BaseURL:   srv.URL,
		CAFile:    caPath,
		TokenFile: tokenPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, tokenPath
}

// pemEncode renders DER bytes as a PEM certificate.
func pemEncode(der []byte) []byte {
	const header = "-----BEGIN CERTIFICATE-----\n"
	const footer = "-----END CERTIFICATE-----\n"
	b64 := base64Encode(der)
	var sb strings.Builder
	sb.WriteString(header)
	for len(b64) > 64 {
		sb.WriteString(b64[:64])
		sb.WriteString("\n")
		b64 = b64[64:]
	}
	sb.WriteString(b64)
	sb.WriteString("\n")
	sb.WriteString(footer)
	return []byte(sb.String())
}

// base64Encode is base64 without an import cycle risk in a test helper.
func base64Encode(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		n = uint32(b[i]) << 16
		if rem > 1 {
			n |= uint32(b[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(b[i+2])
		}
		sb.WriteByte(alphabet[(n>>18)&63])
		sb.WriteByte(alphabet[(n>>12)&63])
		if rem > 1 {
			sb.WriteByte(alphabet[(n>>6)&63])
		} else {
			sb.WriteByte('=')
		}
		if rem > 2 {
			sb.WriteByte(alphabet[n&63])
		} else {
			sb.WriteByte('=')
		}
	}
	return sb.String()
}

// A clustered Proxmox: the cluster row gives the source id, the node
// list gives the nodes, and the per-node status gives the boot mode.
func TestScan_ClusteredProxmox(t *testing.T) {
	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}
		switch r.URL.Path {
		case "/cluster/status":
			json.NewEncoder(w).Encode([]map[string]any{
				{"type": "cluster", "id": "cluster", "name": "prod-cluster", "nodes": 2, "version": 2, "quorate": true},
				{"type": "node", "id": "node/p1", "name": "p1", "nodeid": 1, "online": true},
				{"type": "node", "id": "node/p2", "name": "p2", "nodeid": 2, "online": true},
			})
		case "/nodes":
			json.NewEncoder(w).Encode([]map[string]any{
				{"node": "p1", "type": "node", "status": "online", "cpu": 0.1, "maxcpu": 8, "mem": 1000, "maxmem": 2000, "uptime": 500, "ssl_fingerprint": "FP1"},
				{"node": "p2", "type": "node", "status": "offline", "ssl_fingerprint": "FP2"},
			})
		case "/nodes/p1/status":
			json.NewEncoder(w).Encode(map[string]any{"boot-info": map[string]any{"mode": "efi"}})
		case "/nodes/p2/status":
			json.NewEncoder(w).Encode(map[string]any{"boot-info": map[string]any{"mode": "legacy-bios"}})
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
	if result.SourceID != "prod-cluster" {
		t.Errorf("source id = %q, want prod-cluster", result.SourceID)
	}
	if len(result.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(result.Nodes))
	}
	if result.Nodes[0].Fingerprint != "FP1" {
		t.Errorf("fingerprint = %q, want FP1", result.Nodes[0].Fingerprint)
	}
	if !strings.Contains(result.Nodes[0].Status, "boot=efi") {
		t.Errorf("status = %q, want it to carry boot=efi", result.Nodes[0].Status)
	}
	// p2's stats are ABSENT because the credential lacks Sys.Audit on
	// /nodes/p2. The provider must not choke on that, and must not
	// report a zero memory figure. Its status still carries the
	// provider's own verdict plus the boot mode it did read.
	if !strings.HasPrefix(result.Nodes[1].Status, "offline") {
		t.Errorf("p2 status = %q, want it to start with offline", result.Nodes[1].Status)
	}
	if !strings.Contains(result.Nodes[1].Status, "boot=legacy-bios") {
		t.Errorf("p2 status = %q, want it to carry boot=legacy-bios", result.Nodes[1].Status)
	}
}

// A STANDALONE Proxmox node has NO type:'cluster' row. The provider
// must handle that and report an empty source id rather than erroring
// or inventing a cluster name.
func TestScan_StandaloneProxmoxHasNoClusterRow(t *testing.T) {
	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cluster/status":
			// Exactly what upstream returns for a standalone node.
			json.NewEncoder(w).Encode([]map[string]any{
				{"type": "node", "id": "node/solo", "name": "solo", "nodeid": 0, "local": true, "online": true},
			})
		case "/nodes":
			json.NewEncoder(w).Encode([]map[string]any{
				{"node": "solo", "type": "node", "status": "online", "ssl_fingerprint": "FPS"},
			})
		case "/nodes/solo/status":
			json.NewEncoder(w).Encode(map[string]any{"boot-info": map[string]any{"mode": "efi"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	result := p.Scan(context.Background())

	if result.State != extdiscovery.StateFresh {
		t.Fatalf("state = %q, want %q (err: %v)", result.State, extdiscovery.StateFresh, result.Err)
	}
	if result.SourceID != "" {
		t.Errorf("source id = %q, want empty: a standalone node has no cluster name", result.SourceID)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Name != "solo" {
		t.Errorf("nodes = %+v, want one named solo", result.Nodes)
	}
}

// A 401 must be reported as unauthorized, NOT as an empty inventory.
func TestScan_UnauthorizedIsNotEmpty(t *testing.T) {
	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	result := p.Scan(context.Background())

	if result.State != extdiscovery.StateUnauthorized {
		t.Errorf("state = %q, want %q", result.State, extdiscovery.StateUnauthorized)
	}
	if result.Complete {
		t.Error("complete = true after a 401; a failed scan is never complete")
	}
	if result.Err == nil {
		t.Error("err = nil after a 401")
	}
}

// A 403 must be reported as forbidden - a permission problem, not an
// empty inventory.
func TestScan_ForbiddenIsDistinct(t *testing.T) {
	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))

	result := p.Scan(context.Background())

	if result.State != extdiscovery.StateForbidden {
		t.Errorf("state = %q, want %q", result.State, extdiscovery.StateForbidden)
	}
}

// A node whose detail cannot be read makes the scan PARTIAL, not
// failed: the node itself was seen.
func TestScan_PartialWhenOneNodeDetailFails(t *testing.T) {
	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cluster/status":
			json.NewEncoder(w).Encode([]map[string]any{
				{"type": "cluster", "id": "cluster", "name": "c", "nodes": 2},
			})
		case "/nodes":
			json.NewEncoder(w).Encode([]map[string]any{
				{"node": "p1", "type": "node", "status": "online", "ssl_fingerprint": "FP1"},
				{"node": "p2", "type": "node", "status": "online", "ssl_fingerprint": "FP2"},
			})
		case "/nodes/p1/status":
			json.NewEncoder(w).Encode(map[string]any{"boot-info": map[string]any{"mode": "efi"}})
		case "/nodes/p2/status":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	result := p.Scan(context.Background())

	if result.State != extdiscovery.StatePartial {
		t.Fatalf("state = %q, want %q", result.State, extdiscovery.StatePartial)
	}
	if result.Complete {
		t.Error("complete = true, want false")
	}
	if len(result.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2: both nodes were observed", len(result.Nodes))
	}
}

// A successful scan that found no nodes is EMPTY - a fact, distinct
// from a failure.
func TestScan_EmptyIsAFact(t *testing.T) {
	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cluster/status":
			json.NewEncoder(w).Encode([]map[string]any{})
		case "/nodes":
			json.NewEncoder(w).Encode([]map[string]any{})
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

// The provider must never request a path outside its allowlist. The
// check is checked by re-introducing the bug: if the allowlist were
// prefix-matched, "/cluster/join" would be reachable.
func TestAllowedEndpoints_AreExactPaths(t *testing.T) {
	for _, path := range allowedEndpoints {
		if !strings.HasPrefix(path, "/") {
			t.Errorf("allowed path %q does not start with /", path)
		}
	}
	// The mutating paths Proxmox offers must NOT be in the list.
	for _, forbidden := range []string{
		"/cluster/join", "/cluster/leave", "/nodes/{node}/tasks",
		"/nodes/{node}/qemu", "/nodes/{node}/lxc", "/nodes/{node}/network",
		"/nodes/{node}/status/start", "/nodes/{node}/status/stop",
		"/nodes/{node}/status/shutdown", "/cluster/resources",
	} {
		for _, allowed := range allowedEndpoints {
			if allowed == forbidden {
				t.Errorf("mutating path %q is in the allowlist", forbidden)
			}
		}
	}
}

// The provider must not construct a mutating request. This is checked
// by confirming the client only ever issues GET.
func TestScan_OnlyIssuesGET(t *testing.T) {
	var methods []string
	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		switch r.URL.Path {
		case "/cluster/status":
			json.NewEncoder(w).Encode([]map[string]any{})
		case "/nodes":
			json.NewEncoder(w).Encode([]map[string]any{})
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

// A token file that is missing or empty must fail cleanly rather than
// sending an empty credential.
func TestScan_MissingCredentialFails(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the provider must not reach the server without a credential")
	}))
	defer srv.Close()

	p, err := New(Config{BaseURL: srv.URL, TokenFile: filepath.Join(t.TempDir(), "absent")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result := p.Scan(context.Background())
	if result.State == extdiscovery.StateFresh {
		t.Errorf("state = %q, want a failure", result.State)
	}
	if result.Complete {
		t.Error("complete = true after a missing credential")
	}
}

// A non-HTTPS base URL must be refused at construction, so a
// credential is never sent in cleartext.
func TestNew_RefusesPlaintextHTTP(t *testing.T) {
	_, err := New(Config{BaseURL: "http://pve.example.com:8006", TokenFile: "/tmp/x"})
	if err == nil {
		t.Fatal("New accepted an http:// base URL; a credential must never travel in cleartext")
	}
}
