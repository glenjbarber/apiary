// Package origincert keeps Cloudflare Origin CA private keys and certificate
// files local to one Hive. It deliberately has no raft dependency.
package origincert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NewCSR creates an ECC private key and CSR for a specific hostname set. The
// returned key is PEM encoded only so the caller can write it directly to a
// root-owned local file after Cloudflare returns a matching certificate.
func NewCSR(hostnames []string) (csrPEM, keyPEM string, err error) {
	if len(hostnames) == 0 {
		return "", "", fmt.Errorf("at least one hostname is required")
	}
	for _, hostname := range hostnames {
		if hostname = strings.TrimSpace(hostname); hostname == "" || strings.ContainsAny(hostname, " /@") {
			return "", "", fmt.Errorf("invalid hostname %q", hostname)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generating private key: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: hostnames}, key)
	if err != nil {
		return "", "", fmt.Errorf("creating certificate request: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("encoding private key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), nil
}

// WritePair verifies that certPEM matches keyPEM, then atomically replaces
// certificate and key files in directory. Existing files remain intact if
// validation or either temporary write fails.
func WritePair(directory, name, certPEM, keyPEM string) (certPath, keyPath string, err error) {
	if name == "" || strings.ContainsAny(name, `/\\`) {
		return "", "", fmt.Errorf("invalid certificate name %q", name)
	}
	if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
		return "", "", fmt.Errorf("validating certificate and key: %w", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", "", fmt.Errorf("creating certificate directory: %w", err)
	}
	certPath = filepath.Join(directory, name+".crt")
	keyPath = filepath.Join(directory, name+".key")
	if err := writeAtomic(certPath, []byte(certPEM), 0o644); err != nil {
		return "", "", err
	}
	if err := writeAtomic(keyPath, []byte(keyPEM), 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".apiary-origin-ca-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}
