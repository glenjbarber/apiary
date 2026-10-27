package joincredential

import (
	"encoding/json"
	"testing"
)

// TestVersionThreeCarriesTheColonyCAAndItsLeaf is the property that
// makes ADR-0187's delivery work: a version 3 bundle carries the
// Colony CA, the fingerprint the operator pinned, and the joining
// Comb's own sealed raft transport leaf.
//
// It is a JSON test rather than a behaviour test on purpose. The
// bundle is a WIRE FORMAT, and what matters is that a version 2
// reader - which is what most deployed Combs are - sees a version it
// understands rather than a version it mis-parses.
//
// The check that a version 2 reader refuses a version 3 bundle lives
// in cmd/apiaryctl's installer, which is the joiner.
func TestVersionThreeCarriesTheColonyCAAndItsLeaf(t *testing.T) {
	bundle := Bundle{
		Version:               3,
		RequestID:             "jreq-test",
		NodeID:                "comb.lab3.apiary.work",
		PeerAPIKey:            "peer-key",
		RestartGuardrailToken: "guardrail-token",
		ColonyCA: &ColonyCA{
			CertificatePEM:    "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
			Fingerprint:       "3d28d1c8f04538 5d34f24905fb8d91fb8c835fde6c407592a4d890513345ae4d",
			PinnedFingerprint: "3d28d1c8f045385d34f24905fb8d91fb8c835fde6c407592a4d890513345ae4d",
			SealedLeaf:        "sealed-leaf-envelope",
		},
	}
	body, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var round Bundle
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatal(err)
	}
	if round.Version != 3 {
		t.Errorf("Version = %d, want 3", round.Version)
	}
	if round.ColonyCA == nil {
		t.Fatal("the Colony CA did not survive the round trip")
	}
	if round.ColonyCA.CertificatePEM == "" {
		t.Error("the CA certificate did not survive the round trip")
	}
	if round.ColonyCA.Fingerprint == "" || round.ColonyCA.PinnedFingerprint == "" {
		t.Error("one of the two fingerprints did not survive the round trip")
	}
	if round.ColonyCA.SealedLeaf == "" {
		t.Error("the sealed leaf did not survive the round trip")
	}
}

// TestAVersionOneBundleHasNoColonyCA: a join that is not to be given
// a CA must not carry one, so a version 1 reader sees exactly what it
// expects.
func TestAVersionOneBundleHasNoColonyCA(t *testing.T) {
	body, err := json.Marshal(Bundle{Version: 1, RequestID: "jreq", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	var round Bundle
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatal(err)
	}
	if round.ColonyCA != nil {
		t.Error("a version 1 bundle carries a Colony CA")
	}
}
