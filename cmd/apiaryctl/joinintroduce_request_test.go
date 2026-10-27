package main

import (
	"slices"
	"testing"
)

// join-introduce dials the target directly, so any field of the joiner's
// own state it leaves out is filled in by the target from the TARGET's
// state and recorded as the joiner's. These tests pin the set of fields
// the request must carry.

func TestBuildJoinRequest_CarriesEveryFieldOnlyTheJoinerKnows(t *testing.T) {
	serving := "SHA256:AA:BB"
	raftTransport := "SHA256:CC:DD"
	identity := joinIdentity{nodeID: "brood.example.test", raftBind: "brood.example.test:17600"}

	got := buildJoinRequest(identity, "123456", []string{serving, raftTransport}, 0, 1)

	if got.GetNodeId() != identity.nodeID || got.GetRaftBindAddress() != identity.raftBind {
		t.Errorf("identity = (%q, %q), want (%q, %q)", got.GetNodeId(), got.GetRaftBindAddress(), identity.nodeID, identity.raftBind)
	}
	if got.GetTargetAddress() != "" {
		t.Errorf("TargetAddress = %q, want empty because join-introduce already dialed the target", got.GetTargetAddress())
	}
	if got.GetIntroductionCode() != "123456" {
		t.Errorf("IntroductionCode = %q, want the first code", got.GetIntroductionCode())
	}
	if !slices.Equal(got.GetAdvertisedFingerprints(), []string{serving, raftTransport}) {
		t.Errorf("AdvertisedFingerprints = %v, want both, serving certificate first", got.GetAdvertisedFingerprints())
	}

	// The target fills this in with ITS OWN certificate when it is empty,
	// and records that under this Comb's node ID.
	if got.GetTlsCertFingerprint() != serving {
		t.Errorf("TlsCertFingerprint = %q, want the serving certificate %q", got.GetTlsCertFingerprint(), serving)
	}

	// An empty log is a real, observed zero. Leaving the observation off
	// makes the target measure its own log instead.
	if !got.GetJoinerLogStateObserved() || got.GetJoinerLastLogIndex() != 0 {
		t.Errorf("log state = (observed %v, index %d), want (true, 0)", got.GetJoinerLogStateObserved(), got.GetJoinerLastLogIndex())
	}
}

func TestBuildJoinRequest_ReportsTheJoinersLogIndex(t *testing.T) {
	got := buildJoinRequest(joinIdentity{nodeID: "n", raftBind: "n:17600"}, "000001", []string{"SHA256:AA"}, 7, 1)
	if !got.GetJoinerLogStateObserved() || got.GetJoinerLastLogIndex() != 7 {
		t.Errorf("log state = (observed %v, index %d), want (true, 7)", got.GetJoinerLogStateObserved(), got.GetJoinerLastLogIndex())
	}
}

// TestBuildJoinRequest_ReportsTheJoinersEpoch pins that the joiner
// states its own Colony GENERATION (ADR-0156 Stage 2).
//
// The admission refusal of a stale-epoch node works by comparing the
// joiner's claim against the Colony's own epoch, so a joiner that can
// read its own epoch and does not state it would be opting out of the
// check for no reason - and the case that matters most, a joiner
// restored from a backup taken before the Colony's last generation
// change, is exactly the one that would be opting out.
func TestBuildJoinRequest_ReportsTheJoinersEpoch(t *testing.T) {
	got := buildJoinRequest(joinIdentity{nodeID: "n", raftBind: "n:17600"}, "000001", []string{"SHA256:AA"}, 0, 3)
	if got.GetColonyEpoch() != 3 {
		t.Errorf("ColonyEpoch = %d, want 3 (this Comb's own generation)", got.GetColonyEpoch())
	}
}

// TestBuildJoinRequest_AnAbsentEpochStaysAbsent pins the
// compatibility half: a Comb whose raftd has no epoch recorded states
// none, and the target accepts that unchanged.
func TestBuildJoinRequest_AnAbsentEpochStaysAbsent(t *testing.T) {
	got := buildJoinRequest(joinIdentity{nodeID: "n", raftBind: "n:17600"}, "000001", []string{"SHA256:AA"}, 0, 0)
	if got.GetColonyEpoch() != 0 {
		t.Errorf("ColonyEpoch = %d, want 0 (no claim)", got.GetColonyEpoch())
	}
}

// localCertificates refuses a Comb with no tls_cert, so this cannot
// happen from the command. The helper must still not panic on it, and must
// not invent a fingerprint.
func TestBuildJoinRequest_NoFingerprintsLeavesTheFieldEmpty(t *testing.T) {
	got := buildJoinRequest(joinIdentity{nodeID: "n", raftBind: "n:17600"}, "000001", nil, 0, 1)
	if got.GetTlsCertFingerprint() != "" {
		t.Errorf("TlsCertFingerprint = %q, want empty", got.GetTlsCertFingerprint())
	}
}
