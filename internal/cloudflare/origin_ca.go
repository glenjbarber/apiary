package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/origincert"
)

// OriginCertificate is the non-secret portion of Cloudflare's Origin CA
// issuance response. The certificate PEM is returned separately so callers
// can write it directly to a root-owned file without storing it in raft.
type OriginCertificate struct {
	ID        string
	ExpiresOn string
	PEM       string
}

// OriginCAIssuer adapts Cloudflare's API response to origincert's local
// issuance transaction. It keeps the token in memory only for this request.
type OriginCAIssuer struct{}

func (OriginCAIssuer) Issue(ctx context.Context, token string, hostnames []string, validityDays int, csr string) (origincert.IssuedCertificate, error) {
	cert, err := CreateOriginCertificate(ctx, token, hostnames, validityDays, csr)
	if err != nil {
		return origincert.IssuedCertificate{}, err
	}
	expiresAt, err := time.Parse(time.RFC3339, cert.ExpiresOn)
	if err != nil {
		return origincert.IssuedCertificate{}, fmt.Errorf("parsing Origin CA expiry: %w", err)
	}
	return origincert.IssuedCertificate{ID: cert.ID, ExpiresAt: expiresAt, PEM: cert.PEM}, nil
}

type originCertificateRequest struct {
	Hostnames         []string `json:"hostnames"`
	RequestedValidity int      `json:"requested_validity"`
	RequestType       string   `json:"request_type"`
	CSR               string   `json:"csr"`
}

type originCertificateResult struct {
	ID          string `json:"id"`
	ExpiresOn   string `json:"expires_on"`
	Certificate string `json:"certificate"`
}

// CreateOriginCertificate asks Cloudflare to sign a locally generated CSR.
// token must be a dedicated Zone:SSL and Certificates:Edit token for the
// target zone. Callers retain the private key locally and must never pass it
// here. The API is deliberately not called from reconciliation.
func CreateOriginCertificate(ctx context.Context, token string, hostnames []string, validityDays int, csr string) (OriginCertificate, error) {
	if strings.TrimSpace(token) == "" {
		return OriginCertificate{}, fmt.Errorf("Cloudflare Origin CA token is empty")
	}
	if len(hostnames) == 0 {
		return OriginCertificate{}, fmt.Errorf("at least one hostname is required")
	}
	if strings.TrimSpace(csr) == "" {
		return OriginCertificate{}, fmt.Errorf("certificate signing request is empty")
	}
	if validityDays <= 0 {
		return OriginCertificate{}, fmt.Errorf("requested validity must be positive")
	}

	resp, err := doRequest(ctx, http.MethodPost, "/certificates", token, originCertificateRequest{
		Hostnames:         hostnames,
		RequestedValidity: validityDays,
		RequestType:       "origin-ecc",
		CSR:               csr,
	})
	if err != nil {
		return OriginCertificate{}, fmt.Errorf("creating Origin CA certificate: %w", err)
	}
	var result originCertificateResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return OriginCertificate{}, fmt.Errorf("decoding Origin CA certificate: %w", err)
	}
	if result.ID == "" || result.Certificate == "" {
		return OriginCertificate{}, fmt.Errorf("Cloudflare Origin CA response omitted certificate data")
	}
	return OriginCertificate{ID: result.ID, ExpiresOn: result.ExpiresOn, PEM: result.Certificate}, nil
}
