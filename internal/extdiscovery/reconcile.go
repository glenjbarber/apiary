package extdiscovery

import (
	"fmt"
	"sort"
	"time"
)

// Store holds what Apiary has observed about one provider's world.
//
// It is NOT raft-replicated state, and it is not Apiary's inventory.
// It is a cache of observations with their provenance and freshness -
// the thing a discovery integration is allowed to keep.
//
// The distinction matters because Apiary's inventory IS its raft
// membership (ADR-0191): there is no separate table of "known hosts" to
// write a discovered node into. A discovered host is an observation, and
// observations are kept here.
type Store struct {
	// nodes is keyed by the node's composite identity.
	nodes map[NodeKey]Node

	// resources is keyed by the resource's composite identity.
	resources map[ResourceKey]Resource

	// nodeAbsent records nodes that a COMPLETE scan did not contain.
	// It is what makes absence require CONFIRMATION rather than a
	// single scan's silence.
	nodeAbsent map[NodeKey]bool

	// resourceAbsent is the same for resources.
	resourceAbsent map[ResourceKey]bool

	// lastScan is when the last scan was taken.
	lastScan time.Time

	// state is the last scan's state.
	state State

	// lastErr is the last scan's error, if any.
	lastErr error

	// lastConflicts holds the conflicts from the last applied scan.
	// It is reset on every scan, because a conflict that is still
	// present in the next scan is reported again by that scan, and a
	// historical pile would bury the current one.
	lastConflicts []Conflict

	// sourceID identifies the provider installation the last scan came
	// from. It is EMPTY for a provider that does not cluster, and for
	// a scan that failed - a failed scan did not establish which
	// installation it was talking to.
	sourceID string
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		nodes:          map[NodeKey]Node{},
		resources:      map[ResourceKey]Resource{},
		nodeAbsent:     map[NodeKey]bool{},
		resourceAbsent: map[ResourceKey]bool{},
		state:          StateUnavailable,
	}
}

// Nodes returns the observed nodes, sorted by their composite identity
// so that a caller rendering them gets a stable order.
func (s *Store) Nodes() []Node {
	out := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].SourceID != out[j].SourceID {
			return out[i].SourceID < out[j].SourceID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Resources returns the observed resources, sorted by their composite
// identity.
func (s *Store) Resources() []Resource {
	out := make([]Resource, 0, len(s.resources))
	for _, r := range s.resources {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].SourceID != out[j].SourceID {
			return out[i].SourceID < out[j].SourceID
		}
		if out[i].NodeName != out[j].NodeName {
			return out[i].NodeName < out[j].NodeName
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].SourceResourceID < out[j].SourceResourceID
	})
	return out
}

// State returns the last scan's state.
func (s *Store) State() State { return s.state }

// LastScan returns when the last scan was taken.
func (s *Store) LastScan() time.Time { return s.lastScan }

// LastError returns the last scan's error, if any.
func (s *Store) LastError() error { return s.lastErr }

// SourceID returns the provider installation identifier the last
// complete scan came from.
//
// It is empty for a provider that does not cluster, and for a scan
// that failed. An empty source id is therefore two things - "this
// provider does not cluster" and "the last scan failed" - and the
// scan's STATE is what distinguishes them.
func (s *Store) SourceID() string { return s.sourceID }

// AbsentNodes returns the nodes a complete scan did not contain.
//
// These are reported, not deleted. A node that vanished from a
// provider's listing is very often a node that is down, rebooting, or
// temporarily unreachable - and deleting the record on that evidence
// destroys the history that makes the absence diagnosable.
func (s *Store) AbsentNodes() []NodeKey {
	out := make([]NodeKey, 0, len(s.nodeAbsent))
	for k := range s.nodeAbsent {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].SourceID != out[j].SourceID {
			return out[i].SourceID < out[j].SourceID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Apply folds one scan's result into the store.
//
// The rules, in the order they are applied:
//
//  1. A scan that did not COMPLETE changes nothing except the
//     freshness state. Every record is preserved. This is the rule
//     that stops a failed read from deleting live resources.
//
//  2. A scan that completed updates the records it contained, and
//     marks as absent the records it did not. Absence is recorded
//     here; the caller decides what to do about it.
//
//  3. A record whose fingerprint CHANGED under a matched key is a
//     CONFLICT, not an update. The stored record is left alone.
//
//  4. A record that was seen under a DIFFERENT key but with the same
//     fingerprint is also a conflict - that is a rename, and treating
//     it as a new record would silently orphan the old one.
//
// Nothing is ever deleted by this method. Removal of a record is an
// explicit operator action, not a side-effect of a scan.
func (s *Store) Apply(result ScanResult) {
	s.lastScan = result.ObservedAt
	s.lastErr = result.Err
	s.lastConflicts = nil

	// The source id is recorded even for an incomplete scan, because it
	// is part of what the scan DID establish. A failed scan leaves it
	// empty - a scan that failed did not establish which installation
	// it was talking to.
	if result.Complete {
		s.sourceID = result.SourceID
	}

	// A scan that did not complete is not evidence of anything. The
	// records stay exactly as they were; only the freshness changes.
	//
	// The state is DOWNGRADED here rather than taken as given. A
	// scan that reported itself fresh while also reporting itself
	// incomplete is a contradiction, and resolving it in favour of
	// "fresh" would let a caller treat a half-finished scan as a
	// current one. The safe direction is to report the scan as
	// partial, because partial is the strongest statement that is
	// true of both readings.
	if !result.Complete {
		s.state = result.State
		if s.state == StateFresh || s.state == StateEmpty {
			s.state = StatePartial
		}
		return
	}

	s.state = result.State

	seenNodes := map[NodeKey]bool{}
	seenResources := map[ResourceKey]bool{}

	// fingerprint index, for rename detection.
	fingerprints := map[string]NodeKey{}
	for k, n := range s.nodes {
		if n.Fingerprint != "" {
			fingerprints[n.Fingerprint] = k
		}
	}

	for _, n := range result.Nodes {
		key := n.Key()
		seenNodes[key] = true

		known, exists := s.nodes[key]
		if exists {
			// A fingerprint change under a matched key is a
			// conflict. The node may have been re-keyed, or two
			// different things may now share a name. Either way it
			// is something an operator must look at, so the stored
			// record is left exactly as it was.
			if known.Fingerprint != "" && n.Fingerprint != "" && known.Fingerprint != n.Fingerprint {
				s.recordConflict(Conflict{
					Kind:                "node",
					Key:                 key.String(),
					Reason:              "fingerprint changed for a node whose identity already matched; the node may have been re-keyed, or two different nodes may now share a name",
					ObservedFingerprint: n.Fingerprint,
					KnownFingerprint:    known.Fingerprint,
				})
				continue
			}
			// The ordinary case: same identity, same node. Update
			// the volatile fields and keep everything else.
			known.Status = n.Status
			known.ObservedAt = result.ObservedAt
			if n.Fingerprint != "" {
				known.Fingerprint = n.Fingerprint
			}
			s.nodes[key] = known
			delete(s.nodeAbsent, key)
			continue
		}

		// A new key. If this fingerprint was already known under a
		// different key, this is a RENAME - the same node seen under
		// a new name. It is a conflict, because silently creating a
		// second record would fork the node's history and leave the
		// old record orphaned.
		if n.Fingerprint != "" {
			if prior, ok := fingerprints[n.Fingerprint]; ok && prior != key {
				s.recordConflict(Conflict{
					Kind:                "node",
					Key:                 key.String(),
					Reason:              "a node with this fingerprint is already known under a different identity; the node appears to have been renamed",
					ObservedFingerprint: n.Fingerprint,
					KnownFingerprint:    n.Fingerprint,
				})
				continue
			}
		}

		n.ObservedAt = result.ObservedAt
		s.nodes[key] = n
		if n.Fingerprint != "" {
			fingerprints[n.Fingerprint] = key
		}
		delete(s.nodeAbsent, key)
	}

	for _, r := range result.Resources {
		key := r.Key()
		seenResources[key] = true
		known, exists := s.resources[key]
		if exists {
			known.State = r.State
			known.Name = r.Name
			known.ObservedAt = result.ObservedAt
			s.resources[key] = known
			delete(s.resourceAbsent, key)
			continue
		}
		r.ObservedAt = result.ObservedAt
		s.resources[key] = r
		delete(s.resourceAbsent, key)
	}

	// A complete scan that did not contain a record is evidence of
	// absence. It is recorded, not acted on.
	for key := range s.nodes {
		if !seenNodes[key] {
			s.nodeAbsent[key] = true
		}
	}
	for key := range s.resources {
		if !seenResources[key] {
			s.resourceAbsent[key] = true
		}
	}
}

// recordConflict appends a conflict to the store's last scan conflicts.
//
// The store keeps only the most recent scan's conflicts, because a
// conflict that is still present in the next scan will be reported
// again by that scan, and keeping a historical pile would bury the
// current one.
func (s *Store) recordConflict(c Conflict) {
	s.lastConflicts = append(s.lastConflicts, c)
}

// Conflicts returns the conflicts from the last applied scan.
func (s *Store) Conflicts() []Conflict {
	out := make([]Conflict, len(s.lastConflicts))
	copy(out, s.lastConflicts)
	return out
}

// String renders a node key for a human-readable message.
func (k NodeKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.Provider, k.SourceID, k.Name)
}

// String renders a resource key for a human-readable message.
func (k ResourceKey) String() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", k.Provider, k.SourceID, k.NodeName, k.Kind, k.SourceResourceID)
}
