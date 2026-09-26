// Package restartplan implements ADR-0125's quorum-safe raftd restart
// workflow: the plan/step state machine, the quorum arithmetic, the
// reachability probe, the decision predicates, and the durable record of
// what actually happened - all as pure, injectable, unit-testable
// pieces, with every FreeBSD-specific effect (running `service`, dialing
// a peer, reading a file) behind a small interface this package never
// implements against a live host at import time.
//
// The project convention this package exists to honour is stated most
// plainly in internal/cluster/simulate.go: an unknown or unobserved
// result is a real third state with its own name, and it must never be
// folded into either "fine" or "broken". Every outcome here carries
// that third state explicitly (Outcome's own doc comment), the durable
// record refuses to be written without the evidence its verdict implies
// (ResultStore.Save's validation), and the decision predicates treat a
// probe that could not be read as Unknown rather than Allow, matching
// internal/guardrail's own fail-closed posture.
//
// What is deliberately NOT here: any gRPC/proto surface. ADR-0125 needs
// no new wire messages (its own §9), and the real enforcement stays
// where ADR-0103 put it - the raft-replicated restart-lease FSM apply,
// not any function in this package. The RPC handler in internal/manager
// gathers the facts, calls the pure evaluators here, and submits the
// raft command; see the package's integration notes in plan.go.
package restartplan

import (
	"fmt"
	"strings"
)

// Outcome is the honest classification of what happened to one restart
// attempt. It is deliberately three-plus-way, mirroring
// internal/cluster's own verdict vocabulary (RecoveryVerdict and
// Reachability above all): "we could not tell" is a first-class answer
// with its own name, not a polite way of saying "fine".
//
// There is exactly one place in this codebase where it would be
// tempting to collapse Unknown into something else - a node that was
// restarted and never came back - and it is the one this type exists to
// refuse. IsSuccess and IsFailure are both false for every unknown
// variant, and ResultStore.Save rejects a record whose verdict is not
// backed by the kind of evidence that verdict implies.
type Outcome string

const (
	// OutcomeConfirmed means the restart actually completed AND the
	// node reported itself back healthy (its own startup hook
	// acknowledged the lease, ADR-0125 §2). This is the only outcome
	// that may be rendered as success.
	OutcomeConfirmed Outcome = "confirmed"

	// OutcomeFailed means there is positive evidence the restart did
	// not complete - the service command itself returned a non-zero
	// exit, or the target acknowledged with a failure. Distinct from
	// every unknown below: a failure is a thing that was observed.
	OutcomeFailed Outcome = "failed"

	// OutcomeBlocked means the workflow deliberately refused to act:
	// a guardrail said no (quorum would be lost, a lease is already
	// held, the target is the leader without an acknowledged
	// override). Nothing was attempted, so this is neither success
	// nor failure - it is the absence of an attempt, named as such.
	OutcomeBlocked Outcome = "blocked"

	// OutcomeUnobserved means an attempt was made and the observation
	// came back unusable - the target's raft_bind_address did not
	// answer, the lease state could not be read, the confirmation
	// never arrived. The direct analog of internal/cluster's
	// replica_unobserved ("query WAS attempted and did not return a
	// usable answer").
	OutcomeUnobserved Outcome = "unobserved"

	// OutcomeUnverified means no observation was even possible - the
	// probe itself could not run, the run was cancelled, the record
	// could not be written. The direct analog of internal/cluster's
	// unverified_replica ("no live observation was even attempted").
	// Kept separate from OutcomeUnobserved for exactly the reason
	// internal/cluster keeps them separate: one means we tried and
	// learned nothing, the other means we never got to try.
	OutcomeUnverified Outcome = "unverified"
)

// IsUnknown reports whether o is one of the two honest third states -
// "we could not check" - which is never success and never failure.
func IsUnknown(o Outcome) bool {
	return o == OutcomeUnobserved || o == OutcomeUnverified
}

// IsSuccess reports whether o may be rendered as "the restart worked".
// Only OutcomeConfirmed qualifies; see the type comment.
func IsSuccess(o Outcome) bool { return o == OutcomeConfirmed }

// IsFailure reports whether o may be rendered as "the restart failed",
// i.e. a positive observation of breakage. Only OutcomeFailed
// qualifies: an unreachable node is not evidence of a failed restart.
func IsFailure(o Outcome) bool { return o == OutcomeFailed }

// Result is the durable record of one restart attempt. It is what an
// operator reads afterwards, and what the frontend would render, so
// every field in it has to be true rather than plausible.
//
// Detail and Evidence are the reason the verdict is what it is, and
// ResultStore.Save refuses to persist a record that has a verdict but
// no supporting reason - an unbacked OutcomeConfirmed is precisely the
// bug this type's design is meant to make impossible to write down.
type Result struct {
	Service        string   `json:"service"`
	NodeID         string   `json:"node_id"`
	LeaseID        uint64   `json:"lease_id"`
	Attempt        int      `json:"attempt"`
	Outcome        Outcome  `json:"outcome"`
	Detail         string   `json:"detail"`
	Evidence       []string `json:"evidence,omitempty"`
	StartedAtUnix  int64    `json:"started_at_unix"`
	FinishedAtUnix int64    `json:"finished_at_unix"`
}

// Render produces the one-line human form of a result. An unknown
// outcome renders as unknown, in those words, and the rendering can
// never be mistaken for a confirmed or a failed restart - the whole
// point of the distinct Outcome constants.
func (r Result) Render() string {
	who := r.Service
	if r.NodeID != "" {
		who = r.Service + " on " + r.NodeID
	}
	switch {
	case IsSuccess(r.Outcome):
		return fmt.Sprintf("%s: confirmed healthy after restart (lease %d)", who, r.LeaseID)
	case IsFailure(r.Outcome):
		return fmt.Sprintf("%s: restart FAILED (lease %d) - %s", who, r.LeaseID, r.Detail)
	case r.Outcome == OutcomeBlocked:
		return fmt.Sprintf("%s: restart not attempted - blocked by guardrail (lease %d) - %s", who, r.LeaseID, r.Detail)
	default:
		return fmt.Sprintf("%s: restart outcome UNKNOWN (%s, lease %d) - not a confirmed failure and not a confirmed success: %s", who, r.Outcome, r.LeaseID, r.Detail)
	}
}

// Validate enforces the honesty invariants Result exists to protect:
// every outcome needs a stated reason, and the two positive outcomes
// need actual evidence behind them. Called by ResultStore.Save, and
// exported so a caller rendering a record from somewhere else (an RPC
// handler, a CLI) can check a record it did not write itself.
func (r Result) Validate() error {
	if r.Service == "" {
		return fmt.Errorf("restartplan: result has no service")
	}
	if r.NodeID == "" {
		return fmt.Errorf("restartplan: result has no node_id")
	}
	if strings.TrimSpace(r.Detail) == "" {
		return fmt.Errorf("restartplan: result for %s on %s has outcome %q but no detail - an unbacked verdict is not a record", r.Service, r.NodeID, r.Outcome)
	}
	switch {
	case IsSuccess(r.Outcome):
		if len(r.Evidence) == 0 {
			return fmt.Errorf("restartplan: result claims %q for %s on %s with no evidence - a confirmation must cite what confirmed it", r.Outcome, r.Service, r.NodeID)
		}
	case IsFailure(r.Outcome):
		if len(r.Evidence) == 0 {
			return fmt.Errorf("restartplan: result claims %q for %s on %s with no evidence - a failure must cite what proved it", r.Outcome, r.Service, r.NodeID)
		}
	case r.Outcome == OutcomeBlocked:
	case IsUnknown(r.Outcome):
	default:
		return fmt.Errorf("restartplan: result for %s on %s has unrecognized outcome %q", r.Service, r.NodeID, r.Outcome)
	}
	return nil
}
