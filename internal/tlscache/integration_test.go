package tlscache

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEndToEnd_RealHandshakePicksUpReplacedCertificate drives a real
// TLS listener through Cache.GetCertificate, proving the wiring this
// package exists for: a client dialing in after the pair on disk is
// replaced sees the new certificate, with the listener never
// restarted.
func TestEndToEnd_RealHandshakePicksUpReplacedCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cert1, key1 := generatePair(t, "first")
	writePair(t, certPath, keyPath, cert1, key1)

	cache, err := New(certPath, keyPath, t.Logf)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	lis, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: cache.GetCertificate})
	if err != nil {
		t.Fatalf("tls.Listen() error: %v", err)
	}
	defer lis.Close()

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.(*tls.Conn).Handshake()
			}()
		}
	}()

	dial := func() string {
		conn, err := tls.Dial("tcp", lis.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("tls.Dial() error: %v", err)
		}
		defer conn.Close()
		certs := conn.ConnectionState().PeerCertificates
		if len(certs) == 0 {
			t.Fatal("no peer certificates presented")
		}
		return certs[0].Subject.CommonName
	}

	if got := dial(); got != "first" {
		t.Fatalf("first dial CommonName = %q, want %q", got, "first")
	}

	cert2, key2 := generatePair(t, "second")
	if err := os.WriteFile(certPath, cert2, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, key2, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(certPath, future, future)
	os.Chtimes(keyPath, future, future)

	if got := dial(); got != "second" {
		t.Fatalf("dial after replace CommonName = %q, want %q - the listener did not pick up the renewed certificate", got, "second")
	}
}
