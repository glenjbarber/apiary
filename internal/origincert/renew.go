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
}

// RunOnce checks every locally recorded certificate and renews the ones
// due for it. A failure renewing one certificate does not block the
// others - all failures are joined into the returned error.
func (r *Renewer) RunOnce(ctx context.Context) error {
	directory, tokenFile, err := r.Config()
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
		if _, err := Issue(ctx, r.Issuer, IssueRequest{
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
