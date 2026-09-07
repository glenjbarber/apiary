package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCreateOriginCertificatePostsCSRWithoutPrivateKey(t *testing.T) {
	var got map[string]any
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/certificates" {
			t.Fatalf("request = %s %s, want POST /certificates", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{"id":"cert-1","expires_on":"2030-01-01T00:00:00Z","certificate":"-----BEGIN CERTIFICATE-----\\n..."}`)})
	})

	cert, err := CreateOriginCertificate(context.Background(), "token", []string{"api.example.com"}, 365, "-----BEGIN CERTIFICATE REQUEST-----")
	if err != nil {
		t.Fatal(err)
	}
	if cert.ID != "cert-1" || cert.PEM == "" {
		t.Errorf("certificate = %+v, want issued result", cert)
	}
	if got["request_type"] != "origin-ecc" || got["csr"] == "" {
		t.Errorf("request body = %+v, want ECC CSR request", got)
	}
	if _, ok := got["private_key"]; ok {
		t.Errorf("request body must not contain a private key: %+v", got)
	}
}

func TestCreateOriginCertificateRejectsMissingInputs(t *testing.T) {
	if _, err := CreateOriginCertificate(context.Background(), "", []string{"api.example.com"}, 365, "csr"); err == nil {
		t.Error("missing token succeeded")
	}
	if _, err := CreateOriginCertificate(context.Background(), "token", nil, 365, "csr"); err == nil {
		t.Error("missing hostname succeeded")
	}
}
