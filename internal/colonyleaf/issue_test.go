package colonyleaf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"net"

	"github.com/glenjbarber/apiary/internal/colonyca"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

// testCA creates a Colony CA in a temporary directory.
func testCA(t *testing.T) (dir string, fingerprint string) {
	t.Helper()
	dir = t.TempDir()
	fingerprint, err := colonyca.GenerateCA(dir, time.Now())
	if err != nil {
		t.Fatalf("generating a test CA: %v", err)
	}
	return dir, fingerprint
}

// testRecipient creates a serving certificate for the "joining Comb",
// the way hostcert would.
func testRecipient(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	cert, key, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  "comb.lab3.apiary.work",
		DNSNames:    []string{"comb.lab3.apiary.work"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatalf("generating a test serving certificate: %v", err)
	}
	return string(cert), string(key)
}

// TestIssueProducesALeafThatChainsToTheCA is the whole point: the
// member holding the CA key issues a leaf, and that leaf chains to
// the CA.
func TestIssueProducesALeafThatChainsToTheCA(t *testing.T) {
	dir, fingerprint := testCA(t)
	recipientCert, _ := testRecipient(t)

	sealed, caFP, err := Issue(IssueOptions{
		CADir:                   dir,
		NodeID:                  "comb.lab3.apiary.work",
		RaftBindAddress:         "comb.lab3.apiary.work:17600",
		RequestID:               "jreq-test",
		ExpectedCAFingerprint:   fingerprint,
		RecipientCertificatePEM: recipientCert,
		Now:                     time.Now(),
	})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if caFP != fingerprint {
		t.Errorf("the returned CA fingerprint is %q, want %q", caFP, fingerprint)
	}
	if len(sealed) == 0 {
		t.Fatal("Issue() returned no sealed leaf")
	}
}

// TestOnlyTheCombHoldingTheCAKeyCanIssue is the property that makes
// the CA an anchor: a Comb without the key cannot issue, whatever it
// is asked.
func TestOnlyTheCombHoldingTheCAKeyCanIssue(t *testing.T) {
	_, fingerprint := testCA(t)
	recipientCert, _ := testRecipient(t)
	empty := t.TempDir()

	_, _, err := Issue(IssueOptions{
		CADir:                   empty,
		NodeID:                  "comb.lab3.apiary.work",
		RaftBindAddress:         "comb.lab3.apiary.work:17600",
		RequestID:               "jreq-test",
		ExpectedCAFingerprint:   fingerprint,
		RecipientCertificatePEM: recipientCert,
		Now:                     time.Now(),
	})
	if err == nil {
		t.Fatal("Issue() succeeded on a Comb that holds no CA key")
	}
	// The refusal has to say the Comb cannot issue, because "the file
	// is missing" and "you are not the CA holder" are the same
	// operator problem with two different words.
	if !strings.Contains(err.Error(), "cannot issue a leaf") {
		t.Errorf("refusal %q does not say the Comb cannot issue a leaf", err)
	}
}

// TestIssueRefusesWhenTheExpectedFingerprintIsDifferent is the
// property that stops a forwarded request asking for a leaf from a
// DIFFERENT CA than the Colony pinned.
func TestIssueRefusesWhenTheExpectedFingerprintIsDifferent(t *testing.T) {
	dir, _ := testCA(t)
	_, otherFingerprint := testCA(t)
	recipientCert, _ := testRecipient(t)

	_, _, err := Issue(IssueOptions{
		CADir:                   dir,
		NodeID:                  "comb.lab3.apiary.work",
		RaftBindAddress:         "comb.lab3.apiary.work:17600",
		RequestID:               "jreq-test",
		ExpectedCAFingerprint:   otherFingerprint,
		RecipientCertificatePEM: recipientCert,
		Now:                     time.Now(),
	})
	if err == nil {
		t.Fatal("Issue() issued a leaf for a CA that is not the expected one")
	}
	if !strings.Contains(err.Error(), "not the holder of the CA the Colony pinned") {
		t.Errorf("refusal %q does not explain the CA mismatch", err)
	}
}

// TestIssueRefusesWithoutAnExpectedFingerprint: the holder must not
// issue on somebody's say-so that it holds the right CA.
func TestIssueRefusesWithoutAnExpectedFingerprint(t *testing.T) {
	dir, _ := testCA(t)
	recipientCert, _ := testRecipient(t)

	_, _, err := Issue(IssueOptions{
		CADir:                   dir,
		NodeID:                  "comb.lab3.apiary.work",
		RaftBindAddress:         "comb.lab3.apiary.work:17600",
		RequestID:               "jreq-test",
		RecipientCertificatePEM: recipientCert,
		Now:                     time.Now(),
	})
	if err == nil {
		t.Fatal("Issue() issued a leaf with no expected CA fingerprint")
	}
	if !strings.Contains(err.Error(), "expected CA fingerprint") {
		t.Errorf("refusal %q does not name the missing fingerprint", err)
	}
}

// TestTheLeafIsOnlyReadableByTheJoiner is the property the seal
// exists for: the leaf and its key are readable only by the Comb
// whose certificate they were sealed to.
func TestTheLeafIsOnlyReadableByTheJoiner(t *testing.T) {
	dir, fingerprint := testCA(t)
	recipientCert, recipientKey := testRecipient(t)

	sealed, _, err := Issue(IssueOptions{
		CADir:                   dir,
		NodeID:                  "comb.lab3.apiary.work",
		RaftBindAddress:         "comb.lab3.apiary.work:17600",
		RequestID:               "jreq-test",
		ExpectedCAFingerprint:   fingerprint,
		RecipientCertificatePEM: recipientCert,
		Now:                     time.Now(),
	})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	// The joiner opens it.
	leaf, err := Open(sealed, []byte(recipientCert), []byte(recipientKey), "jreq-test", "comb.lab3.apiary.work")
	if err != nil {
		t.Fatalf("the joiner could not open its own leaf: %v", err)
	}
	if leaf.CertificatePEM == "" || leaf.PrivateKeyPEM == "" {
		t.Error("the opened leaf carries no certificate or no key")
	}

	// A DIFFERENT Comb cannot.
	otherCert, otherKey := testRecipient(t)
	if _, err := Open(sealed, []byte(otherCert), []byte(otherKey), "jreq-test", "comb.lab3.apiary.work"); err == nil {
		t.Fatal("a different Comb opened the leaf")
	}
}

// TestTheLeafIsBoundToItsRequest: a ciphertext copied from one join's
// delivery must fail to decrypt under another's.
func TestTheLeafIsBoundToItsRequest(t *testing.T) {
	dir, fingerprint := testCA(t)
	recipientCert, recipientKey := testRecipient(t)

	sealed, _, err := Issue(IssueOptions{
		CADir:                   dir,
		NodeID:                  "comb.lab3.apiary.work",
		RaftBindAddress:         "comb.lab3.apiary.work:17600",
		RequestID:               "jreq-one",
		ExpectedCAFingerprint:   fingerprint,
		RecipientCertificatePEM: recipientCert,
		Now:                     time.Now(),
	})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if _, err := Open(sealed, []byte(recipientCert), []byte(recipientKey), "jreq-two", "comb.lab3.apiary.work"); err == nil {
		t.Fatal("a leaf opened under a different join request's associated data")
	}
}

// TestTheLeafCarriesTheJoinersOwnNames: the leaf covers the name and
// address the Colony will dial, and nobody else's.
func TestTheLeafCarriesTheJoinersOwnNames(t *testing.T) {
	dir, fingerprint := testCA(t)
	recipientCert, recipientKey := testRecipient(t)

	sealed, _, err := Issue(IssueOptions{
		CADir:                   dir,
		NodeID:                  "comb.lab3.apiary.work",
		RaftBindAddress:         "comb.lab3.apiary.work:17600",
		RequestID:               "jreq-test",
		ExpectedCAFingerprint:   fingerprint,
		RecipientCertificatePEM: recipientCert,
		Now:                     time.Now(),
	})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	leaf, err := Open(sealed, []byte(recipientCert), []byte(recipientKey), "jreq-test", "comb.lab3.apiary.work")
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, colonyca.CACertFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := colonyca.VerifyLeafAgainstCA([]byte(leaf.CertificatePEM), caPEM, []string{"comb.lab3.apiary.work"}, time.Now()); err != nil {
		t.Fatalf("the leaf does not chain to the CA: %v", err)
	}
	// And it must NOT carry another Comb's name.
	if err := colonyca.VerifyLeafAgainstCA([]byte(leaf.CertificatePEM), caPEM, []string{"other.lab3.apiary.work"}, time.Now()); err == nil {
		t.Error("the leaf carries another Comb's name")
	}
}
