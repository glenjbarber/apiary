package frontend

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/certmgr"
	"github.com/glenjbarber/apiary/internal/origincert"
)

// certificateRow is one row of the read-only Certificates page: one
// certificate held by the Comb whose managerd answers this frontend.
//
// It deliberately has no field for a private key, a key path, or any
// certificate PEM. That is not a rendering filter - see
// internal/certmgr's package comment: the check that produces Status
// never opens a *.key file in the first place, so there is nothing
// here that could carry key material even if a template asked for it.
type certificateRow struct {
	Name, Service, Hostnames string
	CertPath                 string
	AutoRenew                bool

	// Status is the checked verdict for this certificate's own
	// remaining lifetime - one of origincert's four ExpiryStatus
	// values. StatusUnknown is a real state, not a rendering failure:
	// it means a check was attempted against the certificate file and
	// could not be completed, and StatusNote carries the reason.
	Status     string
	BadgeClass string
	StatusNote string

	// Expires is the certificate's own NotAfter, read from the file.
	// Empty when Status is unknown, because there is no observed
	// expiry to render in that case.
	Expires string
	// Remaining is the distance to that expiry in words ("in 213
	// days", "expired 3 days ago"). Empty when Status is unknown.
	Remaining string
	// ValidFor is the certificate's own notBefore-to-notAfter span, so
	// an operator can see the lifetime that was actually issued rather
	// than inferring it from a single date.
	ValidFor string
	// NotBefore is the start of that span, for the same reason.
	NotBefore string

	// RecordedExpires/RecordedAt are the inventory's own copy of the
	// expiry and the time it was written. They are shown as context and
	// clearly labelled as recorded, never as the verdict: a value
	// copied into an inventory file is a claim about a certificate, and
	// the whole point of this page is to say whether the certificate
	// still agrees with it. "Never recorded" is rendered rather than
	// hidden, because an entry with no recorded expiry is a gap an
	// operator should see, not an empty cell.
	RecordedExpires string
	RecordedAt      string
}

// certificateExpiryBadgeClass maps a checked certificate verdict onto
// this project's already-defined badge CSS classes (see
// web/templates/layout.html) - no new CSS. The important one is the
// default: ExpiryUnknown must never pick up a healthy colour, because
// "the check could not be made" rendered green is precisely the
// false-negative this page exists to prevent (ADR-0056, ADR-0118).
// Every other class here matches origincert's own spelling, so the same
// certificate reads the same way on the Machine Configuration page.
func certificateExpiryBadgeClass(status origincert.ExpiryStatus) string {
	switch status {
	case origincert.ExpiryOK:
		return "ok"
	case origincert.ExpirySoon:
		return "soon"
	case origincert.ExpiryExpired:
		return "expired"
	default: // origincert.ExpiryUnknown
		return "unknown"
	}
}

// describeCertificateRemaining renders the distance between now and a
// real expiry in words, rounding down rather than up so the text can
// never claim more time than exists. A verdict of unknown never reaches
// this function - there is no observed expiry to describe.
func describeCertificateRemaining(expiresAt, now time.Time) string {
	d := expiresAt.Sub(now)
	switch {
	case d <= 0:
		if -d < 24*time.Hour {
			return "expired less than a day ago"
		}
		return fmt.Sprintf("expired %d days ago", int(-d.Hours()/24))
	case d < 24*time.Hour:
		return "expires in under a day"
	default:
		return fmt.Sprintf("%d days left", int(d.Hours()/24))
	}
}

// describeCertificateValidity renders a certificate's own lifetime span.
// A span under a day is reported as hours rather than as "0 days", which
// would read as a broken value rather than a short one.
func describeCertificateValidity(leaf certmgr.Leaf) string {
	if leaf.NotBefore.IsZero() || leaf.NotAfter.IsZero() {
		return ""
	}
	d := leaf.NotAfter.Sub(leaf.NotBefore)
	if d < 24*time.Hour {
		return fmt.Sprintf("valid for %d hours", int(d.Hours()))
	}
	return fmt.Sprintf("valid for %d days", int(d.Hours()/24))
}

// currentCertificates builds the page's rows from this Comb's own Origin
// CA inventory plus a local read of each certificate file.
//
// Two separate sources, deliberately kept distinct:
//
//   - ListOriginCertificates returns the non-secret inventory managerd
//     holds (identity, recorded expiry, auto-renew flag, file paths).
//     It is a claim about certificates, made when they were installed.
//   - certmgr.Observe re-derives the lifetime by reading the
//     certificate file itself. It is the check, and it is the only thing
//     that produces a Status.
//
// The page renders the check as the verdict and the inventory as
// labelled context. When the check cannot be made the row is unknown and
// says why; the recorded expiry is still shown next to it, because an
// operator debugging an unreadable file wants to know what was last
// believed about it.
//
// Fail-soft, like currentClusterISOs/currentNetworks/currentPlacementHives:
// a fetch that fails yields no rows plus a message, and the page still
// renders. This surface is read-only, so there is no action that could
// fail and no state that could be left half-applied.
func (s *Server) currentCertificates(r *http.Request, now time.Time) ([]certificateRow, string) {
	resp, err := s.client.ListOriginCertificates(r.Context(), &rpcpb.ListOriginCertificatesRequest{})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		return nil, resp.GetError()
	}
	certs := resp.GetCertificates()
	out := make([]certificateRow, 0, len(certs))
	for _, cert := range certs {
		// The inventory entry is rebuilt here purely as the identity
		// and file path the check needs. Its recorded ExpiresAt is
		// deliberately not carried into certmgr.Observe, which would
		// otherwise be one refactor away from quietly falling back to
		// the unverified copy whenever a file read fails.
		entry := origincert.InventoryEntry{
			Name:      cert.GetName(),
			Service:   cert.GetService(),
			Hostnames: cert.GetHostnames(),
			CertPath:  cert.GetCertPath(),
		}
		obs := certmgr.Observe(entry, now)
		row := certificateRow{
			Name: obs.Name, Service: obs.Service,
			Hostnames: strings.Join(obs.Hostnames, ", "),
			CertPath:  cert.GetCertPath(),
			AutoRenew: cert.GetAutoRenew(),
			Status:    string(obs.Status), BadgeClass: certificateExpiryBadgeClass(obs.Status),
			RecordedExpires: "never recorded",
			RecordedAt:      "never recorded",
		}
		if unix := cert.GetExpiresAtUnix(); unix > 0 {
			row.RecordedExpires = time.Unix(unix, 0).Local().Format("2006-01-02 15:04 MST")
		}
		if unix := cert.GetUpdatedAtUnix(); unix > 0 {
			row.RecordedAt = time.Unix(unix, 0).Local().Format("2006-01-02 15:04 MST")
		}
		if obs.Status != origincert.ExpiryUnknown {
			row.Expires = obs.NotAfter.Local().Format("2006-01-02 15:04 MST")
			row.Remaining = describeCertificateRemaining(obs.NotAfter, now)
			row.NotBefore = obs.NotBefore.Local().Format("2006-01-02")
			row.ValidFor = describeCertificateValidity(certmgr.Leaf{NotBefore: obs.NotBefore, NotAfter: obs.NotAfter})
		}
		row.StatusNote = certificateStatusNote(obs)
		out = append(out, row)
	}
	return out, ""
}

// certificateStatusNote is the one sentence explaining a row's verdict,
// including the reason for an unknown. An unknown row's reason is the
// whole point of the row: "unknown" alone tells an operator nothing
// they can act on, so the underlying read error is carried through
// verbatim rather than replaced with a generic message.
func certificateStatusNote(obs certmgr.Observation) string {
	switch obs.Status {
	case origincert.ExpiryOK:
		return "Checked against the certificate file just now: more than " + certmgr.ExpiryWindow.String() + " of life remains."
	case origincert.ExpirySoon:
		return "Checked against the certificate file just now: within " + certmgr.ExpiryWindow.String() + " of expiry."
	case origincert.ExpiryExpired:
		return "Checked against the certificate file just now: this certificate's own validity has already ended."
	default:
		return "Expiry could not be determined: " + obs.Error + ". This is not an expired certificate and not a healthy one - the check did not complete."
	}
}

// handleCertificatesPage serves the read-only Certificates page
// ("/certificates"). It shows which operator-facing certificates this
// Comb holds, when each one expires, and how close that is.
//
// Read-only by design: renewal already exists as origincert.Renewer.RunOnce
// and issuance as the Machine Configuration page's own form, and adding a
// button here that triggers either is a separate decision about who may
// cause a serving service to restart. Nothing on this page changes
// anything.
//
// The page is honest about its own scope. There is no cluster-wide
// certificate inventory - ListOriginCertificates answers only for the
// managerd that receives it, and no peer-forwarding path exists for it -
// so this is one Comb's view, and the page says so rather than implying
// a Colony-wide answer. The raft-replicated cross-Combs inventory ADR-0133
// describes is unimplemented, and rendering a single Comb's rows under a
// cluster-wide heading would be the exact "stale or partial copy presented
// as the whole truth" failure that ADR is written against.
func (s *Server) handleCertificatesPage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	rows, fetchErr := s.currentCertificates(r, now)

	status, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	scope := "This frontend's own Comb"
	if statusErr != nil {
		scope = "This frontend's own Comb (node ID unconfirmed: " + statusErr.Error() + ")"
	} else if id := status.GetManagerNodeId(); id != "" {
		scope = "Comb " + id
	}

	// One message for both failure shapes, because from an operator's
	// point of view they are the same statement: nothing is known here.
	// The distinction that matters - a check that failed versus a
	// certificate that failed its check - is preserved per row by
	// certificateStatusNote.
	errMsg := fetchErr
	if errMsg != "" {
		errMsg = "The certificate inventory for " + scope + " could not be read, so no certificate state is known here: " + errMsg
	}

	s.render(w, "certificates_page", s.withAuthFieldsFrom(r, pageData{
		ActivePage:       "certificates",
		Certificates:     rows,
		CertificateScope: scope,
		CertificateError: errMsg,
		ExpiryWindowNote: "A certificate is reported as expiring once it has less than " + certmgr.ExpiryWindow.String() +
			" of life left, the same window this Comb's own renewal loop acts on. That is a reporting threshold, not a safety margin - nothing degrades at that boundary.",
	}, status, statusErr))
}
