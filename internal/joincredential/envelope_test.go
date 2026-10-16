package joincredential

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/hostcert"
)

func TestSealOpenECDSAAndRSA(t *testing.T) {
	for _, tc := range []struct {
		name string
		pair func(t *testing.T) ([]byte, []byte)
	}{
		{name: "ecdsa", pair: ecdsaPair},
		{name: "rsa", pair: rsaPair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, key := tc.pair(t)
			plain := []byte(`{"peer_api_key":"apk-test","restart_guardrail_token":"token-test"}`)
			aad := []byte("join-request/request-123/node-7")
			sealed, err := Seal(cert, plain, aad)
			if err != nil {
				t.Fatal(err)
			}
			if string(sealed) == string(plain) {
				t.Fatal("envelope contains plaintext")
			}
			opened, err := Open(sealed, cert, key, aad)
			if err != nil {
				t.Fatal(err)
			}
			if string(opened) != string(plain) {
				t.Fatalf("Open() = %q, want original plaintext", opened)
			}
			if _, err := Open(sealed, cert, key, []byte("different-request")); err == nil {
				t.Fatal("Open() with different associated data succeeded")
			}
		})
	}
}

func TestOpenRefusesMismatchedPrivateKey(t *testing.T) {
	cert, _ := ecdsaPair(t)
	_, otherKey := ecdsaPair(t)
	sealed, err := Seal(cert, []byte("secret"), []byte("request"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(sealed, cert, otherKey, []byte("request")); err == nil {
		t.Fatal("Open() accepted a private key unrelated to the certificate")
	}
}

func TestDerivePeerAPIKeyIsStableAndRequestBound(t *testing.T) {
	one, err := DerivePeerAPIKey("colony-token", "jreq-1234", "comb-7")
	if err != nil {
		t.Fatal(err)
	}
	two, err := DerivePeerAPIKey("colony-token", "jreq-1234", "comb-7")
	if err != nil {
		t.Fatal(err)
	}
	other, err := DerivePeerAPIKey("colony-token", "jreq-1235", "comb-7")
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatal("same token and request did not derive the same key")
	}
	if one == other {
		t.Fatal("different requests derived the same key")
	}
}

func ecdsaPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	cert, key, err := hostcert.Generate(hostcert.GenerateOptions{CommonName: "joiner.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func rsaPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "legacy-joiner.example.test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
