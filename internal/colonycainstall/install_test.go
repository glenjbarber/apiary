package colonycainstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/colonyca"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

// testCA creates a Colony CA in a temporary directory and returns its
// certificate PEM, its key PEM, and its fingerprint.
func testCA(t *testing.T) (certPEM string, keyPEM string, fingerprint string) {
	t.Helper()
	dir := t.TempDir()
	fingerprint, err := colonyca.GenerateCA(dir, time.Now())
	if err != nil {
		t.Fatalf("generating a test CA: %v", err)
	}
	cert, err := os.ReadFile(filepath.Join(dir, colonyca.CACertFile))
	if err != nil {
		t.Fatalf("reading the generated CA: %v", err)
	}
	key, err := os.ReadFile(filepath.Join(dir, colonyca.CAKeyFile))
	if err != nil {
		t.Fatalf("reading the generated CA key: %v", err)
	}
	return string(cert), string(key), fingerprint
}

// TestInstallProvisionsTheCATheOperatorPinned is the happy path, and
// the property that matters: the CA that lands on disk is the one
// whose fingerprint the operator compared.
func TestInstallProvisionsTheCATheOperatorPinned(t *testing.T) {
	certPEM, _, fingerprint := testCA(t)
	dir := t.TempDir()

	ca := &joincredential.ColonyCA{
		CertificatePEM:    certPEM,
		Fingerprint:       fingerprint,
		PinnedFingerprint: fingerprint,
	}
	if err := Install(ca, fingerprint, dir, time.Now()); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(dir, colonyca.CACertFile))
	if err != nil {
		t.Fatalf("the CA was not installed: %v", err)
	}
	if !sameCertificate(installed, []byte(certPEM)) {
		t.Error("the installed CA is not the certificate that was pinned")
	}
}

// TestInstallRefusesACADeeplyPinnedAsSomethingElse is the property
// the whole fingerprint exists for: a CA that satisfies every
// structural check but is NOT the one the operator compared must be
// refused.
//
// The refused CA here is a perfectly good CA - it is a CA, it is
// inside its validity window, it parses. It is simply not the pinned
// one. That is exactly the case a structural check alone would let
// through.
func TestInstallRefusesACADeeplyPinnedAsSomethingElse(t *testing.T) {
	certPEM, _, _ := testCA(t)
	_, _, otherFingerprint := testCA(t)
	dir := t.TempDir()

	ca := &joincredential.ColonyCA{
		CertificatePEM:    certPEM,
		Fingerprint:       otherFingerprint,
		PinnedFingerprint: otherFingerprint,
	}
	err := Install(ca, otherFingerprint, dir, time.Now())
	if err == nil {
		t.Fatal("Install() accepted a CA whose fingerprint is not the pinned one")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("refusal %q does not mention the fingerprint, so the operator is not told which check refused", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, colonyca.CACertFile)); statErr == nil {
		t.Error("a CA was written even though the fingerprint did not match")
	}
}

// TestInstallRefusesWhenTheBundlesOwnFingerprintDisagrees covers the
// case where the bundle says one thing and its own certificate says
// another. The joiner is checking two values, and they have to agree.
func TestInstallRefusesWhenTheBundlesOwnFingerprintDisagrees(t *testing.T) {
	certPEM, _, fingerprint := testCA(t)
	dir := t.TempDir()

	ca := &joincredential.ColonyCA{
		CertificatePEM:    certPEM,
		Fingerprint:       fingerprint,
		PinnedFingerprint: fingerprint,
	}
	// Corrupt the bundle's own view of the fingerprint.
	ca.Fingerprint = strings.Repeat("0", len(fingerprint))
	err := Install(ca, fingerprint, dir, time.Now())
	if err == nil {
		t.Fatal("Install() accepted a bundle whose fingerprint disagrees with its own certificate")
	}
	if !strings.Contains(err.Error(), "fingerprints as") {
		t.Errorf("refusal %q does not explain the disagreement", err)
	}
}

// TestInstallNeverReplacesAnExistingCA is the property that keeps a
// retry from breaking a working Colony. Replacing a CA silently
// invalidates every leaf the Colony holds.
func TestInstallNeverReplacesAnExistingCA(t *testing.T) {
	firstPEM, _, firstFingerprint := testCA(t)
	secondPEM, _, secondFingerprint := testCA(t)
	dir := t.TempDir()

	if err := Install(&joincredential.ColonyCA{
		CertificatePEM: firstPEM, Fingerprint: firstFingerprint, PinnedFingerprint: firstFingerprint,
	}, firstFingerprint, dir, time.Now()); err != nil {
		t.Fatalf("installing the first CA: %v", err)
	}

	// A DIFFERENT CA must be refused.
	err := Install(&joincredential.ColonyCA{
		CertificatePEM: secondPEM, Fingerprint: secondFingerprint, PinnedFingerprint: secondFingerprint,
	}, secondFingerprint, dir, time.Now())
	if err == nil {
		t.Fatal("Install() replaced an existing CA with a different one")
	}
	if !strings.Contains(err.Error(), "already holds a different Colony CA") {
		t.Errorf("refusal %q does not say the CA already exists", err)
	}
	installed, _ := os.ReadFile(filepath.Join(dir, colonyca.CACertFile))
	if !sameCertificate(installed, []byte(firstPEM)) {
		t.Error("the existing CA was replaced despite the refusal")
	}

	// The SAME CA must be a success, so a retried join is not blocked
	// by its own earlier progress.
	if err := Install(&joincredential.ColonyCA{
		CertificatePEM: firstPEM, Fingerprint: firstFingerprint, PinnedFingerprint: firstFingerprint,
	}, firstFingerprint, dir, time.Now()); err != nil {
		t.Fatalf("re-installing the SAME CA was refused: %v", err)
	}
}

// TestInstallNeverWritesTheCAKey is the property that keeps the
// Colony's root of trust where it belongs. A joining Comb holds the
// CA CERTIFICATE - enough to verify every peer - and never the key
// that could mint a new identity in the Colony.
func TestInstallNeverWritesTheCAKey(t *testing.T) {
	certPEM, _, fingerprint := testCA(t)
	dir := t.TempDir()

	if err := Install(&joincredential.ColonyCA{
		CertificatePEM: certPEM, Fingerprint: fingerprint, PinnedFingerprint: fingerprint,
	}, fingerprint, dir, time.Now()); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, colonyca.CAKeyFile)); err == nil {
		t.Error("the CA private key was written; a joining Comb must never hold it")
	}
}

// TestInstallRefusesACertificateThatIsNotACA is the structural
// check: a leaf certificate is not an anchor, whatever its
// fingerprint.
func TestInstallRefusesACertificateThatIsNotACA(t *testing.T) {
	certPEM, keyPEM, _ := testCA(t)
	dir := t.TempDir()

	// A well-formed certificate that is NOT a CA: a leaf.
	leafPEM, _, err := colonyca.IssueLeaf([]byte(certPEM), []byte(keyPEM), colonyca.LeafRequest{
		DNSNames: []string{"comb.lab3.apiary.work"},
	}, time.Now())
	if err != nil {
		t.Fatalf("issuing a test leaf: %v", err)
	}
	leafFingerprint, err := colonyca.Fingerprint(leafPEM)
	if err != nil {
		t.Fatal(err)
	}
	err = Install(&joincredential.ColonyCA{
		CertificatePEM: string(leafPEM), Fingerprint: leafFingerprint, PinnedFingerprint: leafFingerprint,
	}, leafFingerprint, dir, time.Now())
	if err == nil {
		t.Fatal("Install() accepted a leaf certificate as a Colony CA")
	}
	if !strings.Contains(err.Error(), "certificate authority") {
		t.Errorf("refusal %q does not say the certificate is not a CA", err)
	}
}
