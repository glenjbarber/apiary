package raft

import (
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/colonyca"
	"github.com/hashicorp/raft"
)

// TestColonyCALeafAcceptedByRaftMutualTLS takes the path an operator
// takes. It makes a Colony CA and a leaf with internal/colonyca, loads
// them through the real newTLSConfig, and runs a real mutual handshake
// through newTLSStreamLayer. Both sides must verify each other. It does
// not use the test-only newTestCA helper from tls_transport_test.go.
func TestColonyCALeafAcceptedByRaftMutualTLS(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	if _, err := colonyca.GenerateCA(dir, now); err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	caCertPEM, err := os.ReadFile(filepath.Join(dir, colonyca.CACertFile))
	if err != nil {
		t.Fatal(err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(dir, colonyca.CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}

	issue := func(name string) (certPath, keyPath string) {
		t.Helper()
		certPEM, keyPEM, err := colonyca.IssueLeaf(caCertPEM, caKeyPEM, colonyca.LeafRequest{
			DNSNames: []string{name, "localhost"},
			IPs:      []net.IP{net.ParseIP("127.0.0.1")},
		}, now)
		if err != nil {
			t.Fatalf("IssueLeaf(%s): %v", name, err)
		}
		certPath = filepath.Join(t.TempDir(), name+".pem")
		keyPath = filepath.Join(t.TempDir(), name+".key")
		if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		return certPath, keyPath
	}
	caPath := filepath.Join(dir, colonyca.CACertFile)

	serverCert, serverKey := issue("node-a")
	clientCert, clientKey := issue("node-b")

	serverCfg, err := newTLSConfig(serverCert, serverKey, caPath)
	if err != nil {
		t.Fatalf("newTLSConfig(server): %v", err)
	}
	clientCfg, err := newTLSConfig(clientCert, clientKey, caPath)
	if err != nil {
		t.Fatalf("newTLSConfig(client): %v", err)
	}

	layer, err := newTLSStreamLayer("127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("newTLSStreamLayer: %v", err)
	}
	t.Cleanup(func() { layer.Close() })

	// The server side must complete its own handshake, which is where the
	// client certificate is verified (RequireAndVerifyClientCert).
	serverDone := make(chan error, 1)
	go func() {
		conn, err := layer.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			serverDone <- os.ErrInvalid
			return
		}
		serverDone <- tlsConn.Handshake()
	}()

	clientLayer := &tlsStreamLayer{tlsConfig: clientCfg}
	conn, err := clientLayer.Dial(raft.ServerAddress(layer.Addr().String()), 5*time.Second)
	if err != nil {
		t.Fatalf("Dial with a colonyca leaf: %v", err)
	}
	defer conn.Close()

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("server verified the client leaf and failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server handshake did not complete")
	}
}
