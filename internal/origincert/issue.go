package origincert

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// IssuedCertificate is the minimal non-secret result returned by an Origin CA
// API client. Keeping this interface here makes the filesystem transaction
// testable without a network client or a real token.
type IssuedCertificate struct {
	ID        string
	ExpiresAt time.Time
	PEM       string
}

// Issuer signs a CSR with a Cloudflare Origin CA certificate. token is read
// from a local file by the caller and must never be persisted or logged.
type Issuer interface {
	Issue(context.Context, string, []string, int, string) (IssuedCertificate, error)
}

// IssueRequest specifies one explicit, local certificate issuance. Apiary
// never invokes this from reconciliation or guesses hostnames from a Cell.
type IssueRequest struct {
	Directory    string
	Name         string
	Service      string
	Hostnames    []string
	ValidityDays int
	Token        string
	AutoRenew    bool
}

// Issue generates a private key locally, requests a certificate through the
// supplied Issuer, validates and installs the pair, then updates the local
// non-secret inventory. A caller may restart an allowlisted service only after
// this returns successfully.
func Issue(ctx context.Context, issuer Issuer, req IssueRequest) (InventoryEntry, error) {
	if issuer == nil {
		return InventoryEntry{}, fmt.Errorf("Origin CA issuer is not configured")
	}
	if strings.TrimSpace(req.Directory) == "" || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Service) == "" {
		return InventoryEntry{}, fmt.Errorf("certificate directory, name, and service are required")
	}
	if strings.TrimSpace(req.Token) == "" {
		return InventoryEntry{}, fmt.Errorf("Origin CA token is empty")
	}
	csr, key, err := NewCSR(req.Hostnames)
	if err != nil {
		return InventoryEntry{}, err
	}
	issued, err := issuer.Issue(ctx, req.Token, req.Hostnames, req.ValidityDays, csr)
	if err != nil {
		return InventoryEntry{}, fmt.Errorf("issuing Origin CA certificate: %w", err)
	}
	if issued.ID == "" || issued.ExpiresAt.IsZero() || issued.PEM == "" {
		return InventoryEntry{}, fmt.Errorf("Origin CA issuer returned incomplete certificate metadata")
	}
	certPath, keyPath, err := WritePair(req.Directory, req.Name, issued.PEM, key)
	if err != nil {
		return InventoryEntry{}, err
	}
	entry := InventoryEntry{
		Name: req.Name, Service: req.Service, Hostnames: append([]string(nil), req.Hostnames...),
		ID: issued.ID, ExpiresAt: issued.ExpiresAt, CertPath: certPath, KeyPath: keyPath,
		UpdatedAt: time.Now().UTC(), AutoRenew: req.AutoRenew, ValidityDays: req.ValidityDays,
	}
	entries, err := LoadInventory(req.Directory)
	if err != nil {
		return InventoryEntry{}, err
	}
	updated := false
	for i := range entries {
		if entries[i].Name == entry.Name {
			entries[i] = entry
			updated = true
		}
	}
	if !updated {
		entries = append(entries, entry)
	}
	if err := SaveInventory(req.Directory, entries); err != nil {
		return InventoryEntry{}, err
	}
	return entry, nil
}
