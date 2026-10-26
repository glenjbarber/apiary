package raft

// ADR-0170's authorized trust transition.
//
// What these pin, in the order the ADR states it: that a transition
// replaces a certificate as ONE visible act rather than an unpin
// followed by a pin; that a refused transition leaves the previous
// certificate trusted; that both certificates are trusted during the
// overlap; that the overlap is bounded; that the incoming certificate
// is re-derived rather than trusted; and that membership is not
// changed by a certificate changing.

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

func transitionCmd(nodeID string, next *internalpb.TrustedPeer, overlap time.Duration, reason string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_TransitionTrustedPeer{
			TransitionTrustedPeer: &internalpb.TransitionTrustedPeer{
				NodeId:               nodeID,
				Next:                 next,
				OverlapExpiresAtUnix: pinTestNow.Add(overlap).Unix(),
				Reason:               reason,
			},
		},
	}
}

func activateCmd(nodeID, reason string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_ActivateTrustedPeerTransition{
			ActivateTrustedPeerTransition: &internalpb.ActivateTrustedPeerTransition{
				NodeId: nodeID,
				Reason: reason,
			},
		},
	}
}

func abandonCmd(nodeID, reason string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_AbandonTrustedPeerTransition{
			AbandonTrustedPeerTransition: &internalpb.AbandonTrustedPeerTransition{
				NodeId: nodeID,
				Reason: reason,
			},
		},
	}
}

// A transition stages the incoming certificate ALONGSIDE the current
// one. The current certificate is untouched - that is the whole point
// of the overlap, and the reason a transition is not an unpin
// followed by a pin.
func TestTransitionStagesTheIncomingCertificateWithoutDroppingTheCurrentOne(t *testing.T) {
	fsm := NewFSM()
	current := newPinTestPeer(t, "node02", "drone")
	if res := applyPin(t, fsm, 1, pinCmd(current)); res.Error != "" {
		t.Fatalf("pinning: %s", res.Error)
	}

	incoming := newPinTestPeer(t, "node02", "drone")
	// A DIFFERENT certificate: same node, different key.
	incoming.CertPem, incoming.Fingerprint, incoming.NotAfterUnix = generatePinTestCert(t, "drone-rotated", 365*24*time.Hour)

	res := applyPin2(t, fsm, 2, transitionCmd("node02", incoming, time.Hour, "rotation"))
	if res.Error != "" {
		t.Fatalf("transition: %s", res.Error)
	}
	stored := fsm.ListTrustedPeers()
	if len(stored) != 1 {
		t.Fatalf("ListTrustedPeers = %d entries, want 1", len(stored))
	}
	entry := stored[0]
	if entry.GetFingerprint() != current.GetFingerprint() {
		t.Errorf("the current certificate's fingerprint changed to %q; a transition must not drop the certificate the peers already trust", entry.GetFingerprint())
	}
	if entry.GetNext() == nil {
		t.Fatal("no certificate is staged; the transition staged nothing")
	}
	if entry.GetNext().GetFingerprint() != incoming.GetFingerprint() {
		t.Errorf("the staged certificate's fingerprint = %q, want %q", entry.GetNext().GetFingerprint(), incoming.GetFingerprint())
	}
}

// The refusal that matters most: a transition for a node that is NOT
// pinned is refused, because creating a pin here would trust a
// certificate nobody compared.
func TestTransitionRefusesAnUnpinnedNode(t *testing.T) {
	fsm := NewFSM()
	incoming := newPinTestPeer(t, "node02", "drone")
	res := applyPin2(t, fsm, 1, transitionCmd("node02", incoming, time.Hour, "rotation"))
	refuseRefusal(t, res, "is not pinned")
	if len(fsm.ListTrustedPeers()) != 0 {
		t.Error("a refused transition created a pin")
	}
}

// A refused transition leaves the previous certificate exactly as it
// was. This is the property that makes a failed rotation recoverable:
// the Comb keeps a trust anchor instead of losing one.
func TestARefusedTransitionLeavesTheCurrentCertificateTrusted(t *testing.T) {
	fsm := NewFSM()
	current := newPinTestPeer(t, "node02", "drone")
	if res := applyPin(t, fsm, 1, pinCmd(current)); res.Error != "" {
		t.Fatalf("pinning: %s", res.Error)
	}

	// A next certificate whose stated fingerprint does not match its
	// own PEM is refused.
	bad := newPinTestPeer(t, "node02", "drone")
	bad.Fingerprint = "SHA256:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00"
	res := applyPin2(t, fsm, 2, transitionCmd("node02", bad, time.Hour, "rotation"))
	refuseRefusal(t, res, "does not match the digest")

	stored := fsm.ListTrustedPeers()
	if len(stored) != 1 {
		t.Fatalf("ListTrustedPeers = %d entries, want 1", len(stored))
	}
	if stored[0].GetFingerprint() != current.GetFingerprint() {
		t.Error("a refused transition changed the trusted certificate; a failed rotation must leave the Comb with a trust anchor")
	}
	if stored[0].GetNext() != nil {
		t.Error("a refused transition staged a certificate")
	}
}

// The overlap is bounded: a transition with no end is refused, because
// an unbounded staged certificate is a permanent second trust anchor.
func TestTransitionRefusesAnUnboundedOverlap(t *testing.T) {
	fsm := NewFSM()
	current := newPinTestPeer(t, "node02", "drone")
	if res := applyPin(t, fsm, 1, pinCmd(current)); res.Error != "" {
		t.Fatalf("pinning: %s", res.Error)
	}
	incoming := newPinTestPeer(t, "node02", "drone")
	cmd := &internalpb.Command{
		Op: &internalpb.Command_TransitionTrustedPeer{
			TransitionTrustedPeer: &internalpb.TransitionTrustedPeer{
				NodeId: "node02",
				Next:   incoming,
				// No overlap_expires_at_unix at all.
				Reason: "rotation",
			},
		},
	}
	res := applyPin2(t, fsm, 2, cmd)
	refuseRefusal(t, res, "overlap_expires_at_unix")
}

// A second transition for the same node is refused rather than
// replacing the staged one, so the store's state for a node is never
// ambiguous.
func TestTransitionRefusesToReplaceAStagedCertificate(t *testing.T) {
	fsm := NewFSM()
	current := newPinTestPeer(t, "node02", "drone")
	if res := applyPin(t, fsm, 1, pinCmd(current)); res.Error != "" {
		t.Fatalf("pinning: %s", res.Error)
	}
	first := newPinTestPeer(t, "node02", "drone")
	if res := applyPin2(t, fsm, 2, transitionCmd("node02", first, time.Hour, "rotation")); res.Error != "" {
		t.Fatalf("the first transition: %s", res.Error)
	}
	second := newPinTestPeer(t, "node02", "drone")
	res := applyPin2(t, fsm, 3, transitionCmd("node02", second, time.Hour, "another rotation"))
	refuseRefusal(t, res, "already has a staged certificate")
}

// Activation promotes the staged certificate and drops the one it
// replaced - and only then.
func TestActivationPromotesTheStagedCertificateAndDropsTheOldOne(t *testing.T) {
	fsm := NewFSM()
	current := newPinTestPeer(t, "node02", "drone")
	if res := applyPin(t, fsm, 1, pinCmd(current)); res.Error != "" {
		t.Fatalf("pinning: %s", res.Error)
	}
	incoming := newPinTestPeer(t, "node02", "drone")
	incoming.CertPem, incoming.Fingerprint, incoming.NotAfterUnix = generatePinTestCert(t, "drone-rotated", 365*24*time.Hour)
	if res := applyPin2(t, fsm, 2, transitionCmd("node02", incoming, time.Hour, "rotation")); res.Error != "" {
		t.Fatalf("transition: %s", res.Error)
	}

	res := applyPin2(t, fsm, 3, activateCmd("node02", "the Comb is serving the new certificate"))
	if res.Error != "" {
		t.Fatalf("activation: %s", res.Error)
	}
	entry := fsm.ListTrustedPeers()[0]
	if entry.GetFingerprint() != incoming.GetFingerprint() {
		t.Errorf("the trusted fingerprint is %q, want the staged one %q", entry.GetFingerprint(), incoming.GetFingerprint())
	}
	if entry.GetNext() != nil {
		t.Error("the staged certificate is still staged after activation")
	}
}

// Abandonment is the failure path: a staged certificate that turns out
// to be wrong is dropped, and the Comb carries on with the certificate
// its peers already trust.
func TestAbandonmentKeepsTheCurrentCertificate(t *testing.T) {
	fsm := NewFSM()
	current := newPinTestPeer(t, "node02", "drone")
	if res := applyPin(t, fsm, 1, pinCmd(current)); res.Error != "" {
		t.Fatalf("pinning: %s", res.Error)
	}
	incoming := newPinTestPeer(t, "node02", "drone")
	if res := applyPin2(t, fsm, 2, transitionCmd("node02", incoming, time.Hour, "rotation")); res.Error != "" {
		t.Fatalf("transition: %s", res.Error)
	}

	res := applyPin2(t, fsm, 3, abandonCmd("node02", "the Comb could not install the staged certificate"))
	if res.Error != "" {
		t.Fatalf("abandonment: %s", res.Error)
	}
	entry := fsm.ListTrustedPeers()[0]
	if entry.GetFingerprint() != current.GetFingerprint() {
		t.Errorf("the trusted fingerprint is %q, want the original %q", entry.GetFingerprint(), current.GetFingerprint())
	}
	if entry.GetNext() != nil {
		t.Error("the staged certificate is still staged after abandonment")
	}
}

// Membership is not changed by a certificate changing. is_voter
// describes whether the Comb is a MEMBER, and a rotation is not a
// join.
func TestTransitionDoesNotChangeMembership(t *testing.T) {
	fsm := NewFSM()
	current := newPinTestPeer(t, "node02", "drone")
	if res := applyPin(t, fsm, 1, pinCmd(current)); res.Error != "" {
		t.Fatalf("pinning: %s", res.Error)
	}
	if res := applyPin(t, fsm, 2, setVoterCmd("node02", true)); res.Error != "" {
		t.Fatalf("promoting to voter: %s", res.Error)
	}
	incoming := newPinTestPeer(t, "node02", "drone")
	incoming.IsVoter = false
	if res := applyPin2(t, fsm, 3, transitionCmd("node02", incoming, time.Hour, "rotation")); res.Error != "" {
		t.Fatalf("transition: %s", res.Error)
	}
	entry := fsm.ListTrustedPeers()[0]
	if !entry.GetIsVoter() {
		t.Error("the staged certificate changed the Comb's membership; a rotation is not a join")
	}
	if entry.GetNext().GetIsVoter() {
		t.Error("the staged certificate claims voter membership of its own accord")
	}
}

// A staged certificate survives a snapshot/restore round trip, so a
// replica catching up from a snapshot sees the same overlap.
func TestStagedCertificateSurvivesRestore(t *testing.T) {
	fsm := NewFSM()
	current := newPinTestPeer(t, "node02", "drone")
	if res := applyPin(t, fsm, 1, pinCmd(current)); res.Error != "" {
		t.Fatalf("pinning: %s", res.Error)
	}
	incoming := newPinTestPeer(t, "node02", "drone")
	if res := applyPin2(t, fsm, 2, transitionCmd("node02", incoming, time.Hour, "rotation")); res.Error != "" {
		t.Fatalf("transition: %s", res.Error)
	}

	encoded, err := proto.Marshal(fsm.SnapshotState())
	if err != nil {
		t.Fatalf("marshalling snapshot state: %v", err)
	}
	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(encoded))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	entry := restored.ListTrustedPeers()[0]
	if entry.GetNext() == nil {
		t.Fatal("the staged certificate did not survive the snapshot; a replica catching up would not see the overlap")
	}
	if entry.GetNext().GetFingerprint() != incoming.GetFingerprint() {
		t.Errorf("the restored staged fingerprint = %q, want %q", entry.GetNext().GetFingerprint(), incoming.GetFingerprint())
	}
}

// applyPin2 applies a command and returns the result. It is a
// distinct name from applyPin because these tests apply several
// different commands to one FSM.
func applyPin2(t *testing.T, fsm *FSM, index uint64, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	result := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, cmd)})
	res, ok := result.(*FSMApplyResult)
	if !ok {
		t.Fatalf("Apply returned %T, want *FSMApplyResult", result)
	}
	return res
}
