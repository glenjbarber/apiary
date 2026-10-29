// Package extdiscovery holds the shared seam for external infrastructure
// discovery providers.
//
// ADR-0191 (Proxmox) and ADR-0192 (Sylve) both decided the same thing
// about their provider, and this package is where that decision lives so
// the two providers cannot drift apart:
//
//   - A provider is READ-ONLY. There is no method here that creates,
//     modifies, or removes anything in a provider's own control plane.
//     That absence is the enforcement of the "no parallel control
//     plane" constraint - a provider that cannot mutate cannot become
//     one, whatever it is later asked to do.
//
//   - A provider is PROVIDER-SHAPED, not provider-specific. The
//     interface below mentions no Proxmox and no Sylve, so a third
//     provider for a different hypervisor or orchestrator implements the
//     same interface. That is what makes the claim "reusable by future
//     infrastructure providers" true rather than aspirational.
//
//   - A DISCOVERY FAILURE IS NOT A DELETION. Every state below
//     distinguishes "this resource is gone" from "we could not see it".
//     A record is only ever marked absent after a COMPLETE successful
//     scan that did not contain it; every other failure preserves the
//     last known record and marks it stale.
//
// The distinction between "absent" and "not seen" is the whole point of
// this package. It is the failure Proxmox's GET /nodes invites (it
// filters rather than errors) and the one Sylve's envelope invites (an
// error response has a null data field). Both are handled by requiring
// a provider to report scan COMPLETENESS separately from scan SUCCESS.
package extdiscovery

import (
	"context"
	"time"
)

// Kind is the resource kind of a discovered record.
//
// "vm" and "jail" are kept distinct even though Apiary represents both
// through one cell model, because a source that can tell them apart
// should not have that distinction erased on the way in. A record that
// loses its kind cannot be compared against a record that kept it.
type Kind string

const (
	// KindVM is a virtual machine.
	KindVM Kind = "vm"
	// KindJail is a jail (container).
	KindJail Kind = "jail"
	// KindNode is a host in the provider's own cluster.
	KindNode Kind = "node"
)

// State is the freshness/completeness state of a discovery scan.
//
// These are deliberately not booleans. A provider that reports
// "reachable: true, healthy: false" cannot distinguish an empty
// inventory from an unreachable one, and every consumer of discovery
// data needs to tell those apart.
type State string

const (
	// StateFresh means the last scan completed successfully and
	// completely. The data is current as of the scan.
	StateFresh State = "fresh"

	// StateStale means the last scan FAILED, but earlier data is
	// retained. The data may be out of date; the resource may still
	// exist.
	StateStale State = "stale"

	// StatePartial means the last scan succeeded but only partially -
	// some nodes or resources could not be read. What was observed is
	// current; what was not observed is UNKNOWN, not absent.
	StatePartial State = "partial"

	// StateUnavailable means the source could not be reached at all -
	// DNS, TCP, TLS, or an unreachable endpoint.
	StateUnavailable State = "unavailable"

	// StateEmpty means a COMPLETE scan succeeded and found nothing.
	// This is a real observation and is NOT the same as unavailable:
	// an empty inventory is a fact, an unavailable source is an absence
	// of facts.
	StateEmpty State = "empty"

	// StateUnauthorized means the credential was rejected or has
	// expired. It is a credential problem, not an inventory problem,
	// and is never retried as if it were transient.
	StateUnauthorized State = "unauthorized"

	// StateForbidden means the credential is valid but lacks permission
	// for what was asked. This is a configuration blocker, NOT an
	// empty inventory - the two are conflated easily and the
	// difference matters to whoever has to fix it.
	StateForbidden State = "forbidden"

	// StateConflict means the source returned something that cannot be
	// placed unambiguously - an identity collision, or a source id
	// that changed under a matched key. It is surfaced for review and
	// never silently merged.
	StateConflict State = "conflict"
)

// Node is one host discovered in a provider's cluster.
//
// A Node is an OBSERVATION of a host the provider manages. It is not
// an Apiary Comb, not a raft member, and not something Apiary
// reconciles against - Apiary's inventory IS its raft membership
// (ADR-0191), and a discovered host is not a member of anything.
//
// The zero value of Node is not a valid observation: a record always
// carries a SourceID and a Name.
type Node struct {
	// Provider is the provider that produced this observation
	// ("proxmox", "sylve"). It is part of the composite identity.
	Provider string `json:"provider"`

	// SourceID identifies the provider installation the node came
	// from - the cluster id when the provider clusters, or the
	// installation's own identity when it does not.
	//
	// It is part of the composite identity precisely so that two
	// unrelated provider installations cannot collide. An EMPTY
	// SourceID is not "the default installation" - it means the
	// provider could not determine one, and the caller must treat
	// that as a distinct case rather than merging it with other
	// empty-keyed records.
	SourceID string `json:"source_id"`

	// Name is the provider's own name for the node.
	Name string `json:"name"`

	// Fingerprint is a stable per-node identity anchor the provider
	// itself supplies - a certificate fingerprint, where the provider
	// exposes one.
	//
	// It is NOT part of the composite key. It is the CONFLICT
	// DETECTION attribute: a record whose key matches but whose
	// fingerprint differs is a conflict, not an update, because it
	// means the node was re-keyed or two things now share a name.
	Fingerprint string `json:"fingerprint,omitempty"`

	// Status is the provider's own status for the node, verbatim. It
	// is NOT normalized into Apiary's vocabulary, because a provider
	// that reports "unknown" and a provider that reports "offline"
	// are different facts and the caller may need to tell them
	// apart. An unrecognised value is preserved, not mapped.
	Status string `json:"status,omitempty"`

	// ObservedAt is when Apiary last saw this node.
	ObservedAt time.Time `json:"observed_at"`
}

// Resource is one guest (VM or jail) discovered in a provider's
// cluster.
//
// A Resource is an OBSERVATION. It is not an Apiary cell, and
// discovery does not adopt it. This is the point at which the two
// providers differ most from each other in what they found: a Proxmox
// guest runs on a host of a kind Apiary cannot manage at all, while a
// Sylve guest is a bhyve VM or a FreeBSD jail - the same kind of thing
// Apiary manages - which makes the temptation to adopt it STRONGER, not
// weaker. The rule is the same for both anyway.
type Resource struct {
	// Provider is the provider that produced this observation.
	Provider string `json:"provider"`

	// SourceID identifies the provider installation.
	SourceID string `json:"source_id"`

	// NodeName is the provider's name for the node the resource lives
	// on.
	NodeName string `json:"node_name"`

	// NodeID is the provider's own stable node identity, where it
	// exposes one (Sylve's nodeUUID). It is the more stable of the
	// two node identifiers, because a hostname can change and a UUID
	// generally cannot.
	NodeID string `json:"node_id,omitempty"`

	// Kind is the resource kind - vm or jail. Never empty.
	Kind Kind `json:"kind"`

	// SourceResourceID is the provider's own identifier for the
	// resource - a RID, a CTID. It is part of the composite identity.
	SourceResourceID string `json:"source_resource_id"`

	// Name is the resource's display name. It is NOT part of the
	// identity: a display name can be reused, and two resources in
	// different clusters can share one.
	Name string `json:"name"`

	// State is the provider's observed state for the resource,
	// verbatim and unnormalized. An unrecognised value is preserved.
	State string `json:"state,omitempty"`

	// ObservedAt is when Apiary last saw this resource.
	ObservedAt time.Time `json:"observed_at"`
}

// NodeKey is the composite identity of a discovered node.
//
// The key is (Provider, SourceID, Name). The provider's fingerprint is
// NOT part of it: the fingerprint is the conflict-detection attribute,
// not the key. Using the fingerprint as the key would make the identity
// unreadable to an operator and would not group a node with its own
// history; using the name alone would collide across installations.
type NodeKey struct {
	Provider string
	SourceID string
	Name     string
}

// ResourceKey is the composite identity of a discovered resource.
//
// The key is (Provider, SourceID, NodeName, Kind, SourceResourceID).
// The node's NAME is used rather than a node UUID because the UUID is
// not always available, and the resource id is scoped to a node anyway.
type ResourceKey struct {
	Provider         string
	SourceID         string
	NodeName         string
	Kind             Kind
	SourceResourceID string
}

// Key returns the node's composite identity.
func (n Node) Key() NodeKey {
	return NodeKey{Provider: n.Provider, SourceID: n.SourceID, Name: n.Name}
}

// Key returns the resource's composite identity.
func (r Resource) Key() ResourceKey {
	return ResourceKey{
		Provider:         r.Provider,
		SourceID:         r.SourceID,
		NodeName:         r.NodeName,
		Kind:             r.Kind,
		SourceResourceID: r.SourceResourceID,
	}
}

// ScanResult is one discovery scan's outcome.
//
// The distinction between Success and Complete is the load-bearing one.
// A scan can succeed and still be incomplete - a partial cluster
// response, one node that could not be reached - and treating that as
// a complete scan is how a discovery integration deletes live
// resources.
//
// A scan that FAILED is never complete. There is no such thing as a
// complete failure.
type ScanResult struct {
	// Nodes are the nodes observed in this scan. An empty list is
	// meaningful only when Complete is true and State is
	// StateEmpty.
	Nodes []Node

	// Resources are the guests observed in this scan.
	Resources []Resource

	// SourceID identifies the provider installation this scan came
	// from - the cluster name for a clustered provider, and the
	// empty string for one that does not cluster.
	//
	// It is empty on a FAILED scan, because a scan that failed did
	// not establish which installation it was talking to.
	SourceID string

	// State is the scan's freshness/completeness state.
	State State

	// Complete is true when the scan saw everything it was supposed
	// to see. It is false for every failure and for every partial
	// result.
	Complete bool

	// ObservedAt is when the scan was taken.
	ObservedAt time.Time

	// Err is the underlying error, when there was one. It is
	// returned to the operator and logged, and never placed in
	// replicated state.
	Err error

	// Conflicts holds records that could not be placed
	// unambiguously - an identity collision, or a source id that
	// changed under a matched key.
	//
	// A conflict is reported and NEVER silently merged. Merging on a
	// hostname or a display name is how two different things become
	// one record, and how a re-keyed node becomes a duplicate.
	Conflicts []Conflict
}

// Conflict describes one record that could not be placed
// unambiguously.
//
// Every field of it is deliberately plain data rather than an error
// type, because a conflict is something an operator has to look at,
// not a condition a caller can retry away.
type Conflict struct {
	// Kind is "node" or "resource".
	Kind string `json:"kind"`

	// Key is the composite identity that was matched.
	Key string `json:"key"`

	// Reason names the conflict in words an operator can act on.
	Reason string `json:"reason"`

	// ObservedFingerprint is the fingerprint seen now, when the
	// conflict is a fingerprint change.
	ObservedFingerprint string `json:"observed_fingerprint,omitempty"`

	// KnownFingerprint is the fingerprint already recorded, when the
	// conflict is a fingerprint change.
	KnownFingerprint string `json:"known_fingerprint,omitempty"`
}

// Provider is the interface every external discovery provider
// implements.
//
// There is deliberately no method here that creates, modifies, or
// removes anything in the provider's own control plane, and no method
// that enrolls anything into Apiary's trust domain. A provider that
// cannot mutate cannot become a second control plane, and a provider
// that cannot enroll cannot grant itself authority.
type Provider interface {
	// Name is the provider's short identifier ("proxmox", "sylve").
	// It is part of every record's composite identity.
	Name() string

	// Scan performs one discovery scan.
	//
	// The returned ScanResult reports what was seen AND how complete
	// the scan was. A provider that cannot complete a scan must
	// return Complete=false and a State that says why - never an
	// empty result that reads as "there is nothing there".
	//
	// An implementation must not return a ScanResult whose State is
	// StateFresh with Complete false, or StateEmpty with Complete
	// true and a non-empty Node list. The states and the completeness
	// flag have to agree.
	Scan(ctx context.Context) ScanResult
}
