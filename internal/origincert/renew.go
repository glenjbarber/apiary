package origincert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Renewer periodically re-issues local Origin CA certificates that have
// AutoRenew set and have entered ExpirySoon or ExpiryExpired. It holds no
// static configuration of its own: Config is called fresh on every RunOnce
// so a directory/token-file path edited through the Machine Configuration
// page takes effect on the next tick, matching every other node-config-
// backed capability in this codebase.
type Renewer struct {
	// Config returns the current Origin CA directory and token-file path.
	// Both empty means the feature is unconfigured on this Comb - RunOnce
	// is then a silent no-op, not an error.
	Config func() (directory, tokenFile string, err error)
	Issuer Issuer
	// RestartService is called with a renewed certificate's own Service
	// name after a successful renewal. May be nil, in which case renewal
	// still happens but no restart is scheduled - the same as a manual
	// renewal whose restart failed (see internal/manager.Server's own
	// IssueOriginCertificate handler).
	RestartService func(service string)
	// Now defaults to time.Now; overridable for tests.
	Now func() time.Time

	// ACMEConfig/ACMEIssuer mirror Config/Issuer for a second,
	// independent local certificate store used by the ACME issuer
	// (ADR-0133 D2/D3, internal/acme). Renewal logic against this store
	// is identical to Config/Issuer's own, just run against a different
	// directory/token file and a different Issuer - nil ACMEConfig (the
	// default, and every pre-existing Renewer value) means this Comb has
	// no ACME certificates to renew, and Origin CA renewal above is
	// completely unaffected either way.
	ACMEConfig func() (directory, tokenFile string, err error)
	ACMEIssuer Issuer
}

// RunOnce checks every locally recorded certificate, in both the Origin
// CA and (if configured) the ACME stores, and renews the ones due for
// it. A failure renewing one certificate, or one whole store, does not
// block any other - all failures are joined into the returned error.
func (r *Renewer) RunOnce(ctx context.Context) error {
	var errs []error
	if err := r.renewStore(ctx, r.Config, r.Issuer); err != nil {
		errs = append(errs, err)
	}
	if r.ACMEConfig != nil {
		if err := r.renewStore(ctx, r.ACMEConfig, r.ACMEIssuer); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Renewer) renewStore(ctx context.Context, config func() (directory, tokenFile string, err error), issuer Issuer) error {
	directory, tokenFile, err := config()
	if err != nil {
		return fmt.Errorf("origincert: loading node config: %w", err)
	}
	if directory == "" || tokenFile == "" {
		return nil
	}
	entries, err := LoadInventory(directory)
	if err != nil {
		return fmt.Errorf("origincert: loading inventory: %w", err)
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	var errs []error
	for _, entry := range entries {
		if !entry.AutoRenew || entry.Expiry(now) == ExpiryOK {
			continue
		}
		token, err := os.ReadFile(tokenFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("origincert: reading token file for %q: %w", entry.Name, err))
			continue
		}
		if _, err := Issue(ctx, issuer, IssueRequest{
			Directory: directory, Name: entry.Name, Service: entry.Service,
			Hostnames: entry.Hostnames, ValidityDays: entry.ValidityDays,
			Token: strings.TrimSpace(string(token)), AutoRenew: true,
		}); err != nil {
			errs = append(errs, fmt.Errorf("origincert: renewing %q: %w", entry.Name, err))
			continue
		}
		if r.RestartService != nil {
			r.RestartService(entry.Service)
		}
	}
	return errors.Join(errs...)
}
