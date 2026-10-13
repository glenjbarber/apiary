// Package acme implements an ACME (RFC 8555) certificate issuer behind
// origincert.Issuer, satisfying ADR-0133 D2's second issuer alongside
// cloudflare.OriginCAIssuer. It validates exclusively through DNS-01,
// publishing the challenge as a Cloudflare DNS TXT record through
// internal/cloudflare - the same zone/token Cloudflare Tunnel exposure
// already uses (ADR-0063) - because that is the only DNS provider this
// codebase already talks to.
//
// Unlike OriginCAIssuer, which needs only the one bearer token
// origincert.Issue passes it per call, an ACME account is a standing
// registration this Comb keeps across issuances. So the account's own
// private key is held by this Issuer value itself, loaded once (or
// generated on first use) and persisted to AccountKeyFile - 0600, in a
// 0700 directory, never replicated through raft in any form. That is
// the same custody rule OriginCATokenFile and CloudflareTokenFile
// already have, and it resolves ADR-0133's open question on account-key
// custody as one account key per Comb, not a Colony-wide shared one: a
// compromised node can only affect its own account's certificates.
package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/glenjbarber/apiary/internal/cloudflare"
	"github.com/glenjbarber/apiary/internal/origincert"
)

// DefaultPropagationWait is how long Issue waits after publishing a
// DNS-01 challenge TXT record before asking the ACME server to validate
// it, giving Cloudflare's own anycast DNS time to propagate the new
// record. A real wait, not a retry budget: the ACME server's own single
// validation attempt would otherwise frequently race a record that is
// real but not yet visible everywhere.
const DefaultPropagationWait = 20 * time.Second

// Issuer implements origincert.Issuer against a real ACME certificate
// authority (Let's Encrypt by default), using DNS-01 validation through
// Cloudflare.
//
// The token origincert.Issue passes to Issue is this Comb's Cloudflare
// DNS API token (the same cloudflare_token_file OriginCATokenFile's own
// call site never touches), read fresh by the caller on every call,
// matching every other token in this codebase. Everything else an ACME
// issuance needs - the account key, the CA's directory URL, the DNS
// zone to publish challenges in - is static configuration of the Issuer
// value itself, constructed once at managerd startup.
type Issuer struct {
	// DirectoryURL is the ACME server's directory endpoint. Empty
	// defaults to Let's Encrypt's production directory
	// (acme.LetsEncryptURL) - set it to a staging endpoint for testing
	// so a mistake does not consume Let's Encrypt's production rate
	// limit.
	DirectoryURL string
	// AccountKeyFile is where this Comb's ACME account private key is
	// persisted (PEM, ECDSA P-256), generated on first use if absent.
	AccountKeyFile string
	// Email is an optional contact address passed at account
	// registration. Let's Encrypt does not require one.
	Email string
	// ZoneID is the Cloudflare zone DNS-01 challenge TXT records are
	// published into - the same zone cloudflare_zone_id already names
	// for Tunnel exposure (ADR-0063).
	ZoneID string
	// PropagationWait overrides DefaultPropagationWait; zero means use
	// the default, not "do not wait".
	PropagationWait time.Duration
}

// Issue requests a certificate for hostnames from the configured ACME
// CA, proving control of each one with a Cloudflare DNS-01 TXT record.
// validityDays is accepted only to satisfy origincert.Issuer's shared
// signature - a public ACME CA decides certificate lifetime unilaterally
// (Let's Encrypt always issues 90-day certificates) and this value is
// otherwise ignored.
func (iss Issuer) Issue(ctx context.Context, token string, hostnames []string, _ int, csrPEM string) (origincert.IssuedCertificate, error) {
	if strings.TrimSpace(iss.ZoneID) == "" {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: cloudflare zone ID is not configured")
	}
	if strings.TrimSpace(iss.AccountKeyFile) == "" {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: account key file is not configured")
	}
	if strings.TrimSpace(token) == "" {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: cloudflare DNS token is empty")
	}
	if len(hostnames) == 0 {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: at least one hostname is required")
	}
	accountKey, err := loadOrCreateAccountKey(iss.AccountKeyFile)
	if err != nil {
		return origincert.IssuedCertificate{}, err
	}

	client := &acme.Client{Key: accountKey, DirectoryURL: iss.DirectoryURL}
	var contact []string
	if iss.Email != "" {
		contact = []string{"mailto:" + iss.Email}
	}
	if _, err := client.Register(ctx, &acme.Account{Contact: contact}, acme.AcceptTOS); err != nil && err != acme.ErrAccountAlreadyExists {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: registering account: %w", err)
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(hostnames...))
	if err != nil {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: creating order: %w", err)
	}

	// Every challenge TXT record this issuance publishes is removed
	// before Issue returns, successfully or not - these are short-lived
	// records specific to one issuance attempt, never left behind for
	// an operator to notice and clean up by hand.
	var cleanups []func()
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()

	wait := iss.PropagationWait
	if wait == 0 {
		wait = DefaultPropagationWait
	}
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return origincert.IssuedCertificate{}, fmt.Errorf("acme: fetching authorization: %w", err)
		}
		if authz.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, c := range authz.Challenges {
			if c.Type == "dns-01" {
				chal = c
				break
			}
		}
		if chal == nil {
			return origincert.IssuedCertificate{}, fmt.Errorf("acme: no dns-01 challenge offered for %s", authz.Identifier.Value)
		}
		value, err := client.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return origincert.IssuedCertificate{}, fmt.Errorf("acme: computing dns-01 challenge record for %s: %w", authz.Identifier.Value, err)
		}
		name := "_acme-challenge." + authz.Identifier.Value
		recordID, err := cloudflare.CreateTXT(ctx, token, iss.ZoneID, name, value)
		if err != nil {
			return origincert.IssuedCertificate{}, fmt.Errorf("acme: publishing dns-01 challenge for %s: %w", authz.Identifier.Value, err)
		}
		host := authz.Identifier.Value
		cleanups = append(cleanups, func() {
			if err := cloudflare.DeleteTXT(context.Background(), token, iss.ZoneID, recordID); err != nil {
				log.Printf("acme: cleaning up dns-01 TXT record for %s: %v", host, err)
			}
		})
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return origincert.IssuedCertificate{}, ctx.Err()
		}
		if _, err := client.Accept(ctx, chal); err != nil {
			return origincert.IssuedCertificate{}, fmt.Errorf("acme: accepting dns-01 challenge for %s: %w", authz.Identifier.Value, err)
		}
		if _, err := client.WaitAuthorization(ctx, authzURL); err != nil {
			return origincert.IssuedCertificate{}, fmt.Errorf("acme: waiting for authorization of %s: %w", authz.Identifier.Value, err)
		}
	}

	csrDER, err := csrDERFromPEM(csrPEM)
	if err != nil {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: decoding CSR: %w", err)
	}
	der, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csrDER, true)
	if err != nil {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: finalizing order: %w", err)
	}
	if len(der) == 0 {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: CA returned no certificate")
	}
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return origincert.IssuedCertificate{}, fmt.Errorf("acme: parsing issued certificate: %w", err)
	}
	var chainPEM strings.Builder
	for _, certDER := range der {
		if err := pem.Encode(&chainPEM, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
			return origincert.IssuedCertificate{}, fmt.Errorf("acme: encoding issued certificate chain: %w", err)
		}
	}
	return origincert.IssuedCertificate{ID: leaf.SerialNumber.String(), ExpiresAt: leaf.NotAfter, PEM: chainPEM.String()}, nil
}

// csrDERFromPEM converts the PEM-encoded CSR origincert.NewCSR produces
// into the raw DER bytes CreateOrderCert expects.
func csrDERFromPEM(csrPEM string) ([]byte, error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("not a PEM-encoded certificate request")
	}
	return block.Bytes, nil
}

// loadOrCreateAccountKey reads path's PEM-encoded ECDSA account key, or
// generates and persists a new one if the file does not yet exist.
// Writing follows the exact stage/fsync/rename discipline
// origincert.WritePair uses: the key is never visible at path half
// written, and an interrupted generation leaves no file at all rather
// than a truncated one.
func loadOrCreateAccountKey(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "EC PRIVATE KEY" && block.Type != "PRIVATE KEY" {
			return nil, fmt.Errorf("account key file %s is not a PEM-encoded private key", path)
		}
		var key *ecdsa.PrivateKey
		if block.Type == "EC PRIVATE KEY" {
			key, err = x509.ParseECPrivateKey(block.Bytes)
		} else {
			var parsed any
			parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
			if err == nil {
				var ok bool
				key, ok = parsed.(*ecdsa.PrivateKey)
				if !ok {
					return nil, fmt.Errorf("account key file %s does not hold an ECDSA key", path)
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("parsing account key file %s: %w", path, err)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading account key file %s: %w", path, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating account key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encoding account key: %w", err)
	}
	if err := writeKeyFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}
	return key, nil
}

func writeKeyFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating account key directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".apiary-acme-account-*")
	if err != nil {
		return fmt.Errorf("staging account key: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("staging account key: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("staging account key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("staging account key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("staging account key: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("installing account key: %w", err)
	}
	return nil
}
