package origincert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Renewer periodically re-issues local Origin CA certificates that have
// AutoRenew set and have entered ExpirySoon or ExpiryExpired. It holds no
// static configuration of its own: Config is called fresh on every RunOnce
// so a directory/token-file path edited through the Machine Configuration
// page takes effect on the next tick, matching every other node-config-
// backed capability in this codebase.
type Renewer struct {
	// mu guards the per-name renewal guard below, so two ticks
	// racing to renew the same certificate cannot both issue.
	//
	// The write itself is atomic, so a race would not leave a
	// half-written pair - but it would leave TWO issuances, and the
	// second would be a wasted rate-limited request against a public
	// CA. The guard makes the second one a no-op.
	mu sync.Mutex
	// renewing names the certificates currently being renewed.
	renewing map[string]bool
	// Config returns the current Origin CA directory and token-file path.
	// Both empty means the feature is unconfigured on this Comb - RunOnce
	// is then a silent no-op, not an error.
	Config func() (directory, tokenFile string, err error)
	Issuer Issuer
	// Renewed is called after a successful renewal, with the inventory
	// entry that was renewed.
	//
	// It is a NOTIFICATION, not an instruction: nothing about the
	// renewal depends on it, and nothing about the renewed
	// certificate's being served depends on it either - the services
	// pick it up on their next TLS handshake through internal/tlscache.
	//
	// RestartService used to be called here, and that was wrong: it
	// dropped the operator's web session for a renewal that had
	// already taken effect. It is kept so a caller that still wants
	// the old behaviour can have it, but no caller in this tree uses
	// it for that any more.
	Renewed func(entry InventoryEntry)

	// RestartService is called with a renewed certificate's own Service
	// name after a successful renewal. DEPRECATED: it restarts a
	// service that does not need restarting, because the serving
	// certificate is re-read on the next handshake. Use Renewed.
	//
	// May be nil, in which case renewal still happens and no restart
	// is scheduled.
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
		// A certificate already being renewed on this tick is
		// SKIPPED, not queued. The first renewal is in flight and
		// will finish; a second issuance would be a wasted request
		// against a public CA's rate limit for no benefit.
		if !r.beginRenewal(entry.Name) {
			continue
		}
		defer r.endRenewal(entry.Name)
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
		// NO RESTART. The certificate is written, and the services
		// pick it up on their next TLS handshake through
		// internal/tlscache - which is what internal/tlscache exists
		// for.
		//
		// A restart here would drop the operator's web session and
		// take the frontend down for a few seconds, on a schedule, for
		// a renewal that had ALREADY taken effect. The manual issuance
		// path stopped doing this when the hot-reload landed; the
		// renewal path had simply never been brought in line.
		//
		// The callback is kept, because a caller may still want to
		// know a renewal happened - and because removing it outright
		// would change the Renewer's shape for every user of it.
		if r.Renewed != nil {
			r.Renewed(entry)
		}
	}
	return errors.Join(errs...)
}

// beginRenewal claims the renewal of one certificate, reporting
// whether the caller may proceed.
//
// A certificate already being renewed is SKIPPED rather than queued:
// the first renewal is in flight and will finish, and a second
// issuance would be a wasted request against a public CA's rate limit
// for no benefit.
func (r *Renewer) beginRenewal(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.renewing == nil {
		r.renewing = make(map[string]bool)
	}
	if r.renewing[name] {
		return false
	}
	r.renewing[name] = true
	return true
}

// endRenewal releases the claim on one certificate.
func (r *Renewer) endRenewal(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.renewing, name)
}
