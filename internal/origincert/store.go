// Package origincert keeps Cloudflare Origin CA private keys and certificate
// files local to one Hive. It deliberately has no raft dependency.
package origincert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// InventoryEntry contains only the non-secret facts needed to report an
// Origin CA certificate's lifetime. It is local to one Hive and deliberately
// excludes token values, private keys, CSRs, and certificate PEM.
type InventoryEntry struct {
	Name      string    `json:"name"`
	Service   string    `json:"service"`
	Hostnames []string  `json:"hostnames"`
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	CertPath  string    `json:"cert_path"`
	KeyPath   string    `json:"key_path"`
	UpdatedAt time.Time `json:"updated_at"`
}

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
	certTmp, err := writeTemp(filepath.Dir(certPath), []byte(certPEM), 0o644)
	if err != nil {
		return "", "", fmt.Errorf("staging certificate: %w", err)
	}
	defer os.Remove(certTmp)
	keyTmp, err := writeTemp(filepath.Dir(keyPath), []byte(keyPEM), 0o600)
	if err != nil {
		return "", "", fmt.Errorf("staging private key: %w", err)
	}
	defer os.Remove(keyTmp)
	// Both replacement files are completely written and fsynced before either
	// live path changes. Apiary schedules the consuming service restart only
	// after this function succeeds, so it never reloads a mismatched pair.
	if err := os.Rename(certTmp, certPath); err != nil {
		return "", "", fmt.Errorf("installing certificate: %w", err)
	}
	if err := os.Rename(keyTmp, keyPath); err != nil {
		return "", "", fmt.Errorf("installing private key: %w", err)
	}
	return certPath, keyPath, nil
}

// SaveInventory atomically records the current non-secret certificate
// inventory alongside the certificate pair. It does not inspect or return
// PEM contents.
func SaveInventory(directory string, entries []InventoryEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding certificate inventory: %w", err)
	}
	tmp, err := writeTemp(directory, append(data, '\n'), 0o600)
	if err != nil {
		return fmt.Errorf("staging certificate inventory: %w", err)
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, filepath.Join(directory, "inventory.json")); err != nil {
		return fmt.Errorf("installing certificate inventory: %w", err)
	}
	return nil
}

// LoadInventory returns an empty inventory when it has not yet been created.
func LoadInventory(directory string) ([]InventoryEntry, error) {
	data, err := os.ReadFile(filepath.Join(directory, "inventory.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading certificate inventory: %w", err)
	}
	var entries []InventoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decoding certificate inventory: %w", err)
	}
	return entries, nil
}

func writeTemp(directory string, data []byte, mode os.FileMode) (string, error) {
	tmp, err := os.CreateTemp(directory, ".apiary-origin-ca-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}
