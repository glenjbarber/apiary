// Package colonycainstall provisions a Colony's certificate authority
// onto a joining Comb.
//
// It is the joiner's half of ADR-0155 step 3, and it is its own
// package for the same reason internal/certintegration is: BOTH sides
// of a join need it, and putting the installation in the manager
// package would have made the joiner depend on the manager, which
// drags raft, bbolt and the cluster packages into a binary an operator
// runs during an incident.
//
// The rules are the security properties, and each one is a refusal
// rather than a warning:
//
//   - THE PINNED FINGERPRINT IS CHECKED BEFORE ANYTHING IS WRITTEN.
//     The CA certificate is verified against the fingerprint the
//     operator pinned in the join authorization, and only a CA that
//     matches it is installed. A CA that satisfies every structural
//     check but not the fingerprint is exactly the certificate the
//     comparison exists to catch.
//
//   - AN EXISTING CA IS NEVER REPLACED. A Comb that already holds the
//     Colony's CA keeps it. Replacing a CA silently invalidates every
//     leaf the Colony holds, and a join must not do that.
//
//   - THE CA PRIVATE KEY NEVER ARRIVES AND NEVER LEAVES. It is the
//     Colony's root of trust and it stays on the Comb that created
//     it. This package only ever writes a certificate.
//
//   - THE CA KEY IS NOT CREATED HERE EITHER. A joining Comb holds the
//     CA CERTIFICATE - enough to verify every peer and to be verified
//     by them - and never the key that could mint a new identity in
//     the Colony.
package colonycainstall

import (
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glenjbarber/apiary/internal/colonyca"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

// DefaultCADir is where the CA certificate is installed, alongside the
// raft transport's own material.
//
// It is the same directory the live Colony uses, and the one
// `apiaryctl colony-ca init` writes into.
const DefaultCADir = "/usr/local/etc/apiary/ca"

// Install provisions the Colony CA onto this Comb.
//
// The CA is verified against expectedFingerprint BEFORE anything is
// written, and an existing CA is never replaced.
//
// An existing CA that is the SAME certificate is a success, not a
// refusal: a join that is retried after a partial failure must not be
// blocked by its own earlier progress. An existing CA that is a
// DIFFERENT certificate is a refusal, because a Colony has one trust
// anchor and two of them is a split.
func Install(ca *joincredential.ColonyCA, expectedFingerprint string, caDir string, now time.Time) error {
	if ca == nil {
		return nil
	}
	if caDir == "" {
		caDir = DefaultCADir
	}
	if ca.CertificatePEM == "" {
		return errors.New("the join credential bundle carries a Colony CA with no certificate")
	}
	// The fingerprint the OPERATOR PINNED is what is checked. The
	// bundle's own copy is not trusted for this comparison, because
	// the bundle is the Colony's own output - checking the Colony's
	// certificate against a fingerprint the Colony also supplied would
	// be checking nothing at all.
	//
	// The two are then required to AGREE, so a bundle whose
	// fingerprint does not match its own certificate is refused too.
	if err := colonyca.VerifyCA([]byte(ca.CertificatePEM), expectedFingerprint, now); err != nil {
		return err
	}
	if ca.Fingerprint != "" {
		bundleFingerprint, err := colonyca.Fingerprint([]byte(ca.CertificatePEM))
		if err != nil {
			return err
		}
		if !equalFold(bundleFingerprint, ca.Fingerprint) {
			return fmt.Errorf("the join credential bundle's Colony CA fingerprint is %s but its own certificate fingerprints as %s", ca.Fingerprint, bundleFingerprint)
		}
	}

	certPath := filepath.Join(caDir, colonyca.CACertFile)
	existing, err := os.ReadFile(certPath)
	if err == nil {
		if sameCertificate(existing, []byte(ca.CertificatePEM)) {
			// Already the right CA. A retried join is not a failure.
			return nil
		}
		return fmt.Errorf("this Comb already holds a different Colony CA at %s; a Colony has one trust anchor, and replacing it would invalidate every leaf the Colony holds. Remove it by hand only if this Comb is being rebuilt", certPath)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading the existing Colony CA at %s: %w", certPath, err)
	}
	if err := os.MkdirAll(caDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", caDir, err)
	}
	// The certificate is PUBLIC, so 0644 is right - the same mode
	// `apiaryctl colony-ca init` writes it with. The KEY is never
	// written here at all.
	if err := writeNewFile(certPath, []byte(ca.CertificatePEM), 0o644); err != nil {
		return err
	}
	return nil
}

// sameCertificate reports whether two PEMs hold the same certificate,
// by DER bytes.
//
// A file-path comparison would be wrong: two files can hold the same
// CA, and a fingerprint comparison in the wrong format would call them
// different.
func sameCertificate(a, b []byte) bool {
	aBlock, _ := pem.Decode(a)
	bBlock, _ := pem.Decode(b)
	if aBlock == nil || bBlock == nil {
		return false
	}
	return string(aBlock.Bytes) == string(bBlock.Bytes)
}

// writeNewFile writes body to path with mode, refusing to overwrite.
//
// The refusal matters even here: a CA certificate is published exactly
// once, and a half-written file that read as a valid-but-wrong CA
// would be worse than no file at all.
func writeNewFile(path string, body []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite %s", path)
		}
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return f.Sync()
}

// equalFold compares two fingerprints case-insensitively.
//
// colonyca's own format is lowercase hex, but a value that reached a
// human through a terminal may have been retyped in any case, and
// "the same CA" is a property of the certificate rather than of how
// the fingerprint was typed.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// LeafIsIssuedByCA reports whether leafPEM was signed by the CA in
// caPEM, and whether the leaf carries every one of expectedDNSNames.
//
// It is the check the JOINER makes on its own raft transport leaf
// after adopting the Colony CA: a leaf that does not chain to the CA
// it now trusts is a certificate no peer can verify, and a joiner that
// kept it would be a member the Colony could not authenticate.
func LeafIsIssuedByCA(leafPEM, caPEM []byte, expectedDNSNames []string, now time.Time) error {
	return colonyca.VerifyLeafAgainstCA(leafPEM, caPEM, expectedDNSNames, now)
}
