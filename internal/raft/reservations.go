package raft

// ADR-0171's prepared-certificate reservations: the replicated store.
//
// The store holds METADATA about a prepared certificate - which
// reservation, which FQDN, which fingerprint, bound to which joining
// Comb - and deliberately holds no key material. The prepared
// certificate itself stays in the preparing Comb's protected store and
// is sealed to the joining Comb at delivery time.
//
// Why it is replicated at all: the BINDING is the load-bearing part.
// A reservation bound in one managerd's memory would be lost if that
// managerd stepped aside, and the joining Comb would be left holding a
// reservation it could never collect - a join that cannot complete
// and an operator with nothing to act on.
//
// Every refusal names itself, because each is a different operator
// problem:
//
//   - An unbound reservation is deliverable to nobody, because there
//     is no honest answer to "is this the Comb I prepared this for"
//     until the join attestation has answered it.
//
//   - A reservation cannot be bound twice, because a re-bind would
//     hand the prepared private key to a second machine.
//
//   - A delivered reservation is spent, because the delivery is
//     single-use.

import (
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// applyCreateCertificateReservation records a prepared certificate.
//
// The reservation is keyed by its own identifier, and the identifier is
// what the joining Comb presents, so a duplicate identifier is refused
// rather than replacing an existing reservation: the same reasoning
// the peer trust store's create-only rule uses, for the same reason.
func (f *FSM) applyCreateCertificateReservation(index uint64, cmd *internalpb.CreateCertificateReservation) *FSMApplyResult {
	res := cmd.GetReservation()
	if res == nil {
		return &FSMApplyResult{Index: index, Error: "CreateCertificateReservation: the reservation must be set"}
	}
	if res.GetReservationId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateCertificateReservation: reservation_id must be set"}
	}
	if res.GetFqdn() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateCertificateReservation: fqdn must be set"}
	}
	if res.GetFingerprint() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateCertificateReservation: fingerprint must be set"}
	}
	if res.GetCreatedAtUnix() <= 0 {
		return &FSMApplyResult{Index: index, Error: "CreateCertificateReservation: created_at_unix must be set"}
	}
	if res.GetExpiresAtUnix() <= 0 {
		return &FSMApplyResult{Index: index, Error: "CreateCertificateReservation: expires_at_unix must be set; an unbounded reservation holds a prepared key indefinitely"}
	}
	if _, exists := f.certificateReservations[res.GetReservationId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"CreateCertificateReservation: reservation %q already exists; a preparation is made once and bound once",
			res.GetReservationId())}
	}
	f.certificateReservations[res.GetReservationId()] = proto.Clone(res).(*internalpb.CertificateReservation)
	return &FSMApplyResult{Index: index, CertificateReservation: f.certificateReservations[res.GetReservationId()]}
}

// applyBindCertificateReservation binds a reservation to the joining
// Comb and the join request that identifies it.
//
// It is refused if the reservation is already bound, and refused if the
// node does not match the reservation's own. A prepared private key
// has two holders until the joiner replaces it; making it three would
// defeat the point.
//
// bound_at_unix is an INPUT rather than a wall-clock read, for the
// determinism reason every other timestamp in this file is: this
// function runs on every voter and again on every replay, so
// consulting time.Now() here would let a replay years later reject an
// entry that originally succeeded and leave replicas disagreeing.
func (f *FSM) applyBindCertificateReservation(index uint64, cmd *internalpb.BindCertificateReservation) *FSMApplyResult {
	id := cmd.GetReservationId()
	if id == "" {
		return &FSMApplyResult{Index: index, Error: "BindCertificateReservation: reservation_id must be set"}
	}
	if cmd.GetNodeId() == "" {
		return &FSMApplyResult{Index: index, Error: "BindCertificateReservation: node_id must be set"}
	}
	if cmd.GetRequestId() == "" {
		return &FSMApplyResult{Index: index, Error: "BindCertificateReservation: request_id must be set"}
	}
	if cmd.GetBoundAtUnix() <= 0 {
		return &FSMApplyResult{Index: index, Error: "BindCertificateReservation: bound_at_unix must be set"}
	}
	existing, exists := f.certificateReservations[id]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("BindCertificateReservation: reservation %q does not exist", id)}
	}
	if existing.GetNodeId() != "" {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"BindCertificateReservation: reservation %q is already bound to node %q for request %q; a prepared private key is made for one joining Comb",
			id, existing.GetNodeId(), existing.GetRequestId())}
	}
	updated := proto.Clone(existing).(*internalpb.CertificateReservation)
	updated.NodeId = cmd.GetNodeId()
	updated.RequestId = cmd.GetRequestId()
	updated.BoundAtUnix = cmd.GetBoundAtUnix()
	f.certificateReservations[id] = updated
	return &FSMApplyResult{Index: index, CertificateReservation: updated}
}

// applyDeliverCertificateReservation marks a reservation as delivered.
//
// A delivered reservation is SPENT. That is what makes the delivery
// single-use: the state moves once and never moves back, so a second
// collection is refused by the state machine rather than by each
// caller remembering to check.
func (f *FSM) applyDeliverCertificateReservation(index uint64, cmd *internalpb.DeliverCertificateReservation) *FSMApplyResult {
	id := cmd.GetReservationId()
	if id == "" {
		return &FSMApplyResult{Index: index, Error: "DeliverCertificateReservation: reservation_id must be set"}
	}
	if cmd.GetDeliveredAtUnix() <= 0 {
		return &FSMApplyResult{Index: index, Error: "DeliverCertificateReservation: delivered_at_unix must be set"}
	}
	existing, exists := f.certificateReservations[id]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("DeliverCertificateReservation: reservation %q does not exist", id)}
	}
	if existing.GetNodeId() == "" {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"DeliverCertificateReservation: reservation %q is not bound to a joining Comb; an unbound reservation is deliverable to nobody", id)}
	}
	if existing.GetDeliveredAtUnix() != 0 {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"DeliverCertificateReservation: reservation %q has already been delivered; the prepared pair is single-use", id)}
	}
	updated := proto.Clone(existing).(*internalpb.CertificateReservation)
	updated.DeliveredAtUnix = cmd.GetDeliveredAtUnix()
	f.certificateReservations[id] = updated
	return &FSMApplyResult{Index: index, CertificateReservation: updated}
}

// applyDropCertificateReservation removes a reservation outright.
//
// It is how a withdrawn or expired preparation is cleared, and how a
// request that reached a terminal state without collecting its
// reservation is cleaned up. The reason is replicated with the
// removal, for the same reason UnpinTrustedPeer records one.
func (f *FSM) applyDropCertificateReservation(index uint64, cmd *internalpb.DropCertificateReservation) *FSMApplyResult {
	id := cmd.GetReservationId()
	if id == "" {
		return &FSMApplyResult{Index: index, Error: "DropCertificateReservation: reservation_id must be set"}
	}
	existing, exists := f.certificateReservations[id]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("DropCertificateReservation: reservation %q does not exist", id)}
	}
	delete(f.certificateReservations, id)
	return &FSMApplyResult{Index: index, CertificateReservation: existing}
}

// ListCertificateReservations returns every reservation, sorted by
// reservation ID.
//
// Sorted rather than in map order, for the same reason
// ListTrustedPeers sorts: both callers are byte comparators, and a
// list that reorders itself between renders looks like a store that is
// mutating under the operator.
func (f *FSM) ListCertificateReservations() []*internalpb.CertificateReservation {
	f.mu.Lock()
	defer f.mu.Unlock()

	ids := make([]string, 0, len(f.certificateReservations))
	for id := range f.certificateReservations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*internalpb.CertificateReservation, 0, len(ids))
	for _, id := range ids {
		out = append(out, proto.Clone(f.certificateReservations[id]).(*internalpb.CertificateReservation))
	}
	return out
}

// CertificateReservationExpired reports whether a reservation's
// delivery window has closed as of now.
//
// A predicate, not a sweep, for the same reason TrustedPeerExpired is
// one: an expired reservation is still IN the store, and every reader
// decides the same way from the same replicated field. Nothing has to
// run for the store to be correct.
func CertificateReservationExpired(r *internalpb.CertificateReservation, now int64) bool {
	return r.GetExpiresAtUnix() <= now
}

// reservationExpiryCheck is a small helper the manager uses, kept
// here so the expiry rule lives next to the store it applies to.
func reservationExpiryCheck(r *internalpb.CertificateReservation, now time.Time) bool {
	return CertificateReservationExpired(r, now.Unix())
}

// bindCertificateReservationForApproval binds a reservation to a
// joining Comb and a join request, as part of the approval.
//
// It is called from the approve arm rather than as a command of its
// own, because the approval is the moment the Colony decides which
// Comb is joining. A reservation bound by an earlier command could
// have been bound to a request that was later rejected.
//
// It returns an empty string on success and a reason on failure. The
// approval is REFUSED rather than accepted without the binding: a
// reservation that stays unbound is a preparation nobody can collect
// and nobody can clear.
//
// The refusals are the same shape as applyBindCertificateReservation's,
// and for the same reasons - with one addition. The node the
// reservation is bound to must be the node being approved, which is
// what stops a preparation for one Comb from being handed to another.
func (f *FSM) bindCertificateReservationForApproval(reservationID, nodeID, requestID string, boundAtUnix int64) string {
	if reservationID == "" {
		return "the prepared certificate reservation ID must be set"
	}
	if nodeID == "" {
		return "the joining node's ID must be set"
	}
	if requestID == "" {
		return "the join request ID must be set"
	}
	if boundAtUnix <= 0 {
		return "the binding timestamp must be set"
	}
	existing, exists := f.certificateReservations[reservationID]
	if !exists {
		return fmt.Sprintf("prepared certificate reservation %q does not exist", reservationID)
	}
	if existing.GetNodeId() != "" {
		return fmt.Sprintf("prepared certificate reservation %q is already bound to node %q for request %q; a prepared private key is made for one joining Comb",
			reservationID, existing.GetNodeId(), existing.GetRequestId())
	}
	if existing.GetDeliveredAtUnix() != 0 {
		return fmt.Sprintf("prepared certificate reservation %q has already been delivered", reservationID)
	}
	updated := proto.Clone(existing).(*internalpb.CertificateReservation)
	updated.NodeId = nodeID
	updated.RequestId = requestID
	updated.BoundAtUnix = boundAtUnix
	f.certificateReservations[reservationID] = updated
	return ""
}
