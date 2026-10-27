package colonycainstall

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/colonyca"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

// TestTheJoinerEndsUpWithACAVerifiesItsOwnLeaf is the whole point,
// end to end.
//
// A joining Comb receives a CA and a pinned fingerprint, installs the
// CA, and then needs to be able to ISSUE a raft transport leaf that
// its peers will accept. The only way to know that works is to
// actually issue the leaf against the installed CA and verify it the
// way a peer would - with Go's own verifier, not by inspection.
//
// The chain this exercises is the one ADR-0155 describes:
//
//	operator compares CA fingerprint by hand
//	  -> fingerprint recorded in the join authorization
//	    -> fingerprint travels in the sealed join bundle
//	      -> joiner verifies the CA against it
//	        -> joiner holds the CA
//	          -> joiner issues a leaf from it
//	            -> peers verify that leaf against the CA
func TestTheJoinerEndsUpWithACAVerifiesItsOwnLeaf(t *testing.T) {
	// The Colony's side: a CA, and the fingerprint the operator
	// compared by hand.
	caCertPEM, caKeyPEM, caFingerprint := testCA(t)

	// The joiner receives it in the bundle, with the pinned value.
	bundle := joincredential.ColonyCA{
		CertificatePEM:    caCertPEM,
		Fingerprint:       caFingerprint,
		PinnedFingerprint: caFingerprint,
	}

	// The joiner's side: it verifies and installs.
	joinerCADir := t.TempDir()
	if err := Install(&bundle, bundle.PinnedFingerprint, joinerCADir, time.Now()); err != nil {
		t.Fatalf("the joiner could not install the Colony CA: %v", err)
	}
	installedPEM, err := os.ReadFile(filepath.Join(joinerCADir, colonyca.CACertFile))
	if err != nil {
		t.Fatalf("the joiner has no CA after the install: %v", err)
	}

	// The joiner now issues its own raft transport leaf from the CA
	// it was given.
	leafPEM, _, err := colonyca.IssueLeaf(installedPEM, []byte(caKeyPEM), colonyca.LeafRequest{
		DNSNames: []string{"comb.lab3.apiary.work"},
	}, time.Now())
	if err != nil {
		t.Fatalf("the joiner could not issue a leaf from the CA it was given: %v", err)
	}

	// And that leaf must verify against the CA, the way a peer would.
	if err := LeafIsIssuedByCA(leafPEM, installedPEM, []string{"comb.lab3.apiary.work"}, time.Now()); err != nil {
		t.Fatalf("the leaf the joiner issued does not verify against the CA it was given: %v", err)
	}
}

// TestALeafFromADifferentCADoesNotVerify is the property that makes
// the CA an anchor rather than a formality: a leaf signed by a
// DIFFERENT CA must not verify against the one the joiner was given.
//
// Without this, a joining Comb would accept any certificate that
// happened to be handed to it, which is the failure mode the pinned
// fingerprint exists to prevent.
func TestALeafFromADifferentCADoesNotVerify(t *testing.T) {
	installedPEM, _, _ := testCA(t)
	otherCertPEM, otherKeyPEM, _ := testCA(t)

	leafPEM, _, err := colonyca.IssueLeaf([]byte(otherCertPEM), []byte(otherKeyPEM), colonyca.LeafRequest{
		DNSNames: []string{"comb.lab3.apiary.work"},
	}, time.Now())
	if err != nil {
		t.Fatalf("issuing a test leaf: %v", err)
	}
	err = LeafIsIssuedByCA(leafPEM, []byte(installedPEM), []string{"comb.lab3.apiary.work"}, time.Now())
	if err == nil {
		t.Fatal("a leaf from a different CA verified against the Colony CA")
	}
}
