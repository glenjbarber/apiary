// Package extnodes holds Apiary's record of ENROLLED external hosts.
//
// ADR-0191 (Proxmox) and ADR-0192 (Sylve) established the discovery
// half of external integration: a read-only provider observes another
// management plane's hosts. That half lives in internal/extdiscovery.
//
// This package is the other half, and it exists because of a fact
// ADR-0191's Context section establishes: **Apiary's inventory IS its
// raft membership.** There is no separate table of "known hosts" to
// write a discovered node into, and a discovered host is not a member of
// anything. A Proxmox node is a Linux host running QEMU/KVM; an Apiary
// Comb is a FreeBSD host running bhyve. Putting the second in the
// first's model would claim a management relationship that does not
// exist.
//
// So an enrolled external node is a record HERE, and it is deliberately
// NOT:
//
//   - a raft member. It holds no vote and is not reachable over Apiary's
//     raft transport. The enrollment path has no call to AddVoter and no
//     access to the raft membership command surface, so a discovered
//     node cannot become a raft member through any sequence of Apiary
//     operations. That is a property of the code, not a rule an operator
//     is asked to respect.
//
//   - a Comb. It has no managerd, no bhyve, no ZFS datasets, no cells.
//
//   - a management target. An enrolled node is a KNOWN, AUTHENTICATED,
//     OPERATOR-APPROVED EXTERNAL HOST that Apiary can report on and
//     associate Apiary-side objects with. Apiary does not create, start,
//     stop, migrate or snapshot workloads on it. That would need a
//     Linux/KVM management plane, which is a separate product decision
//     and a separate ADR.
//
// WHAT ENROLLMENT MEANS HERE, precisely:
//
// An enrolled record says "an operator, in two separate authorizing acts
// on two separate Colony members, admitted this external host to this
// Colony's reporting domain". It grants visibility and association. It
// grants no management authority over the host, and no authority over
// the provider's own control plane.
//
// THE STORE IS LOCAL, NOT REPLICATED.
//
// This mirrors the discovery store's reasoning, and the two reasons
// still hold. A provider credential is a per-host secret, and a secret
// does not belong in replicated state. And an ENROLLMENT IS NOT A
// COLONY FACT in the same way a membership is: the enrolled host's
// identity is a provider-scoped composite key, and the decision to
// admit it is an operator decision that other Combs do not need to
// reproduce.
//
// It DOES need to be consistent enough that two Combs do not disagree
// about who is enrolled, though. So the record carries the authorizations
// that produced it, and the API surface reports what each Comb knows,
// and a discrepancy is visible rather than silent. See
// Enrollments and discrepancies below.
package extnodes

import (
	"fmt"
	"time"
)

// Key is the composite identity of an enrolled external node.
//
// It is (Provider, SourceID, Name) - the same composite the discovery
// store uses, and for the same reasons: the provider's node name is
// stable within an installation and is what an operator reads off a
// provider's own screen, and the source id keeps two unrelated
// installations from colliding.
//
// The provider's fingerprint is NOT part of the key. It is the
// CONFLICT-DETECTION attribute: a record whose key matches but whose
// fingerprint differs is a conflict, not an update, because it means the
// node was re-keyed or two things now share a name.
type Key struct {
	// Provider is the provider that produced the observation
	// ("proxmox", "sylve").
	Provider string

	// SourceID identifies the provider installation. It is empty for a
	// provider that does not cluster, which is a fact about the source
	// rather than a missing value.
	SourceID string

	// Name is the provider's own name for the node.
	Name string
}

// String renders the key for a human-readable message.
func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%s", k.Provider, k.SourceID, k.Name)
}

// Node is one host DISCOVERED in a provider's world.
//
// It mirrors internal/extdiscovery's own Node, and it exists here so
// that the enrollment path can name a host it has checked, without
// importing the discovery package's store into this one.
//
// It is an OBSERVATION, not an enrollment. Nothing in this package acts
// on it.
type Node struct {
	// Key is the composite identity.
	Key Key

	// Fingerprint is the provider's stable per-node identity anchor,
	// where the provider exposes one.
	Fingerprint string

	// Status is the provider's own status for the node, verbatim and
	// unnormalized.
	Status string
}

// Enrollment is one enrolled external host.
//
// It records that the host was admitted, by whom, when, and what was
// observed about it at the time. It does NOT record a management
// relationship, and nothing in this codebase acts on it as though it
// did.
type Enrollment struct {
	// Key is the composite identity.
	Key Key

	// Fingerprint is the provider's stable per-node identity anchor,
	// captured at enrollment. It is what makes a rename detectable as
	// a conflict rather than silently forking a record.
	Fingerprint string

	// Status is the provider's own status for the node, captured at
	// enrollment, verbatim and unnormalized.
	Status string

	// EnrolledAt is when the second authorizing act was recorded.
	EnrolledAt time.Time

	// Authorizations are the two authorizing acts that produced this
	// record: two distinct API keys, presented to two distinct Colony
	// members.
	//
	// They are carried on the record rather than only in the audit
	// log so that an operator reading the enrolled set can see WHO
	// admitted each host without correlating against a log that may
	// have rotated.
	Authorizations []Authorization

	// EnrolledBy names the enrollment's origin, so an operator can
	// tell afterwards which hosts came from a provider and which were
	// joined by hand. ADR-0191 requires this to be "a distinct, visible
	// act in the log, distinguishable from a manual join".
	EnrolledBy string
}

// Authorization is one authorizing act.
//
// It mirrors the shape of the join approval attestation in ADR-0147
// Part 3, because it is the same rule applied to a different target: a
// distinct API key, presented to a distinct Colony member.
type Authorization struct {
	// KeyID is the API key that authorized the act.
	KeyID string

	// NodeID is the Colony member the act was presented to.
	NodeID string

	// AtUnix is when the act was recorded.
	AtUnix int64
}

// Status describes an enrolled host's current standing.
//
// These are deliberately not booleans, for the same reason the discovery
// states are not: a reader has to tell "still there" from "was there"
// from "could not be checked".
type Status string

const (
	// StatusEnrolled means the host is enrolled and was last seen.
	StatusEnrolled Status = "enrolled"

	// StatusAbsent means a COMPLETE discovery scan did not contain the
	// host any more. The record is KEPT - a host that vanished from a
	// provider's listing is very often a host that is down, rebooting,
	// or temporarily unreachable, and deleting the record on that
	// evidence destroys the history that makes the absence
	// diagnosable. Removal is an explicit operator action.
	StatusAbsent Status = "absent"

	// StatusConflict means the host was seen under a matched key with a
	// DIFFERENT fingerprint, or seen under a different key with the
	// same fingerprint. Either way it cannot be placed unambiguously
	// and is surfaced for review.
	StatusConflict Status = "conflict"

	// StatusUnenrolled means the host was removed by an explicit
	// operator action.
	StatusUnenrolled Status = "unenrolled"
)
