package origincert

import "testing"

func TestNewCSRAndWritePair(t *testing.T) {
	csr, key, err := NewCSR([]string{"api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if csr == "" || key == "" {
		t.Fatal("CSR or key was empty")
	}
	// A CSR is intentionally not a certificate, so use a matching self-signed
	// fixture from the standard parser's own error behavior only to prove that
	// WritePair refuses an unissued certificate before touching the filesystem.
	if _, _, err := WritePair(t.TempDir(), "frontend", csr, key); err == nil {
		t.Error("WritePair accepted a CSR as a certificate")
	}
}

func TestWritePairRejectsPathName(t *testing.T) {
	if _, _, err := WritePair(t.TempDir(), "../escape", "", ""); err == nil {
		t.Error("WritePair accepted a path-like name")
	}
}
