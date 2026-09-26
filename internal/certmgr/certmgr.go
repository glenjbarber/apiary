// Package certmgr turns a certificate file that already exists on one Comb
// into an honest expiry observation. It reads; it never writes, never issues,
// and never renews.
//
// It exists because internal/origincert already records everything an operator
// needs about a certificate's lifetime as non-secret InventoryEntry facts, and
// nothing so far has checked that recorded expiry against the certificate
// actually on disk right now. That gap is the one that matters: a recorded
// expiry says "a file was valid until this date when it was installed", and
// says nothing about whether the file still exists, still parses, or is still
// the certificate it was recorded for. A page that renders only the recorded
// value therefore reports a confident answer to a question it did not ask.
//
// Two properties are load-bearing and are what the rest of this codebase may
// rely on:
//
//   - Every function here is a local filesystem read of the certificate only.
//     None of them opens a socket. That is what keeps a certificate verdict
//     separable from a reachability verdict (ADR-0056's ReachabilityUnknown):
//     "this Comb's serving certificate is bad" and "this Comb cannot reach
//     its peers" are different facts with different blast radii (ADR-0133 D5),
//     and this package can only ever answer the first.
//
//   - Private keys are structurally excluded, not filtered out of a template.
//     No function here accepts a *.key path, no function opens one, and no
//     returned type has a field capable of holding key material. Expiry is a
//     property of the public certificate alone - notBefore and notAfter are in
//     the certificate - so the key is never needed to answer the question, and
//     a code path that cannot read it cannot leak it. This is the same custody
//     rule internal/origincert already follows (a key is generated on the Comb
//     that serves the certificate and never leaves it, ADR-0133 D3), and it is
//     why this package verifies nothing about key/certificate pairing: a
//     mismatch is a separate check, belonging to whoever restarts the serving
//     process, and answering it here would require reading the key for no gain.
package certmgr

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/glenjbarber/apiary/internal/origincert"
)

// ExpiryWindow is the one named threshold in this package: with more than this
// much life left a certificate is reported ExpiryOK, with less it is reported
// ExpirySoon, and with none left it is reported ExpiryExpired.
//
// It is deliberately an alias of origincert.RenewalWindow rather than a second
// literal spelling of 30 days. The number a surface renders and the window the
// renewal loop acts on are the same policy decision, and two constants that
// happen to hold the same value today is exactly how a rendering threshold and
// an acting threshold silently drift apart.
//
// Why 30 days is the policy: a renewal is not a same-day action. Renewing an
// operator-facing certificate means reaching an external issuer with a working
// credential, completing a challenge, and restarting the serving service - and
// Apiary's own renewal tick is measured in hours. Thirty days is long enough
// for that to happen inside a normal operational rhythm and long enough that
// the alert is not the first day someone hears about it. It is a reporting
// threshold, not a safety margin: nothing degrades at 30 days, a certificate
// still serving on day 29 is still serving, and a shorter window would mostly
// produce pages that are re-read during an incident. It also matches the
// 7-day floor the shortest Origin CA validity allows
// (internal/cloudflare.ValidOriginCAValidity), so the shortest-lived
// certificate this codebase can issue still gets a real renewal attempt rather
// than appearing inside the window already at issue.
const ExpiryWindow = origincert.RenewalWindow

// Leaf is the parsed public half of a certificate file: the facts about its
// lifetime that an operator-facing page needs, and nothing else.
type Leaf struct {
	NotBefore time.Time
	NotAfter  time.Time
	// Subject is the certificate's own distinguished name, read from the
	// public certificate. It never contains key material - it is the
	// name the issuer put on the public half.
	Subject string
	// DNSNames is the certificate's own SAN list, which for an
	// operator-facing certificate is what an operator actually wants to
	// check against the hostname they meant to protect.
	DNSNames []string
}

// Inspect reads certPath and returns its leaf certificate's lifetime.
//
// It reads the certificate file and nothing else. Every failure - an empty
// path, a missing or unreadable file, content with no CERTIFICATE block,
// a block that will not parse - returns an error, never a zero Leaf and never
// a guess. Callers must treat an error as "the check could not be made", which
// is a real state with its own verdict and is emphatically not "expired".
func Inspect(certPath string) (Leaf, error) {
	if certPath == "" {
		return Leaf{}, fmt.Errorf("no certificate file path is recorded for this certificate")
	}
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return Leaf{}, fmt.Errorf("reading certificate file: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return Leaf{}, fmt.Errorf("certificate file contains no PEM block")
	}
	if block.Type != "CERTIFICATE" {
		return Leaf{}, fmt.Errorf("certificate file's first PEM block is %q, not CERTIFICATE", block.Type)
	}
	certs, err := x509.ParseCertificates(block.Bytes)
	if err != nil {
		return Leaf{}, fmt.Errorf("parsing certificate: %w", err)
	}
	if len(certs) == 0 {
		return Leaf{}, fmt.Errorf("certificate file parsed to zero certificates")
	}
	leaf := certs[0]
	return Leaf{
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Subject:   leaf.Subject.String(),
		DNSNames:  append([]string(nil), leaf.DNSNames...),
	}, nil
}

// Classify compares a real expiry against now and returns the verdict. It is
// the whole policy in one place: ExpiryWindow decides the boundary, and
// nothing else in this package is allowed to decide it.
//
// A zero notAfter means no expiry was ever observed, so it is ExpiryUnknown
// rather than ExpiryExpired. An old caller reaching this with an unset time
// gets the honest answer instead of "expired since the year one".
func Classify(notAfter, now time.Time) origincert.ExpiryStatus {
	switch {
	case notAfter.IsZero():
		return origincert.ExpiryUnknown
	case !notAfter.After(now):
		return origincert.ExpiryExpired
	case notAfter.Before(now.Add(ExpiryWindow)):
		return origincert.ExpirySoon
	default:
		return origincert.ExpiryOK
	}
}

// Observation is one certificate's checked lifetime, as a surface can safely
// render it.
//
// Every field is non-secret by construction: there is no field here for a key,
// a key path, a certificate PEM, or a CSR, so there is nothing to accidentally
// pass to a template. Error is not decoration - it is the evidence for an
// ExpiryUnknown row and is rendered verbatim rather than replaced with a
// generic message, because "the check could not be made" is only actionable if
// the operator can see why.
type Observation struct {
	// Name/Service/Hostnames are identity, carried through from the
	// inventory entry the observation was made about. They describe
	// which certificate this is, not what state it is in.
	Name      string
	Service   string
	Hostnames []string

	// Status is the verdict. ExpiryUnknown is a first-class value here,
	// not an error return: an operator looking at a page needs to see
	// that a check was attempted and could not be completed, and that is
	// a different fact from any verdict about the certificate.
	Status origincert.ExpiryStatus

	// NotBefore/NotAfter are the certificate's own times, zero when
	// Status is ExpiryUnknown. They are the real values read from the
	// file, never the inventory's recorded copy.
	NotBefore time.Time
	NotAfter  time.Time

	// ObservedAt is when this check ran, so a surface can say how fresh
	// a verdict is rather than implying a value is being re-verified
	// continuously between page loads.
	ObservedAt time.Time

	// Error explains an ExpiryUnknown verdict and is empty otherwise. It
	// is the reason the check could not be completed - an unreadable
	// file, unparseable content, or no path at all.
	Error string
}

// Observe performs the complete local self-check for one inventory entry: read
// the certificate file it names, parse it, and classify the lifetime found.
//
// It opens no sockets, so it produces the same verdict whether or not the
// network is up - which is the property that makes the result trustworthy
// rather than a network symptom in disguise (ADR-0133 D5).
//
// Every failure mode - an entry with no certificate path, a deleted file, an
// unreadable one, a truncated one, a file that is not a certificate at all -
// returns ExpiryUnknown with the reason recorded. None of them is reported as
// ExpiryExpired, and none is reported as ExpiryOK either. In particular the
// entry's own recorded ExpiresAt is never used as a fallback: a recorded date
// is a claim about a file, and if the file cannot be read the claim cannot be
// checked, so presenting it as a verdict would be reporting an unverified copy
// as if it were the thing itself.
func Observe(entry origincert.InventoryEntry, now time.Time) Observation {
	obs := Observation{
		Name:       entry.Name,
		Service:    entry.Service,
		Hostnames:  append([]string(nil), entry.Hostnames...),
		Status:     origincert.ExpiryUnknown,
		ObservedAt: now,
	}
	leaf, err := Inspect(entry.CertPath)
	if err != nil {
		obs.Error = err.Error()
		return obs
	}
	obs.NotBefore = leaf.NotBefore
	obs.NotAfter = leaf.NotAfter
	obs.Status = Classify(leaf.NotAfter, now)
	return obs
}
