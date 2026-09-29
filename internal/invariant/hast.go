package invariant

// HAST evidence for the two HAST invariants in this catalog.
//
// ADR-0060 shipped both HAST invariants permanently capped at Unknown
// on a stated premise - "live HAST sync status has no RPC exposure
// anywhere in this codebase" - and disclosed the cap rather than
// quietly dropping it. That premise lapsed when ADR-0119 added
// GetLocalHASTResourceStatus, a deliberately node-local, read-only
// report of one node's own `hastctl list` view, reachable on any peer
// without leader forwarding (internal/manager's PeerReporter), and
// ADR-0121 consumed exactly it to replace the simulator's blanket
// unverified_replica with real in-sync / out-of-sync / unobserved
// verdicts. The cap is lifted here by gathering the evidence that RPC
// already makes available; this file is the pure reading of it.
//
// This package still owns no I/O: HASTObservation is plain data a
// caller fills in (internal/frontend gathers it), and every Evaluate*
// function below issues no RPC and reads no local state.

import (
	"strings"
	"time"
)

// HASTObservation is one node's own live HAST view of one resource,
// gathered by the caller via GetLocalHASTResourceStatus (ADR-0119).
// Role/Status/Replication are the verbatim wire values, never
// anything derived from them.
type HASTObservation struct {
	// NodeID is the node that was asked, for evidence text. It is not
	// itself evidence: an observation's worth comes from Observed.
	NodeID string

	// Attempted separates "no query was possible" from "a query was
	// issued and did not answer" - ADR-0121's own unverified_replica
	// versus replica_unobserved distinction, and the reason Detail
	// reads differently in the two cases. The zero value means no
	// query was made, which is never evidence that the resource is
	// healthy and never evidence that it is broken.
	Attempted bool

	// Observed is true only when the node answered with its own hastctl
	// view. An empty Status with Observed true is a real outcome, not a
	// missing one: internal/hast's parser requires only a `role:` line,
	// so `hastctl list` output carrying a role but no `status:` line
	// parses successfully into an empty status. That reachable case is
	// the whole reason an absent status is treated as silence below
	// rather than as a problem.
	Observed bool
	Role     string
	Status   string

	// Replication is carried verbatim for the operator and is
	// deliberately never interpreted. `hastctl list`'s `dirty` counter
	// is likewise left alone - no explanation here turns any of this
	// into an RPO figure (ADR-0121's own "not fixed here").
	Replication string

	// Detail is the reason a query produced nothing: a transport
	// error, a manager-side refusal, or - when Attempted is false -
	// why no query could be made at all (no peer forwarding, no
	// address, or node identity unavailable).
	Detail string
}

// The role and status spellings this build recognizes. They are the
// same five values internal/cluster/simulate.go already uses for its
// own ADR-0121 verdicts, deliberately restated here rather than
// imported: internal/cluster does not export them, and the two
// packages have no dependency on each other. The reading below is
// deliberately the same reading, in the same order, with the same
// silence rule - see readHAST's own comment for the one-way
// convergence this is owed.
const (
	hastRoleInit       = "init"
	hastRolePrimary    = "primary"
	hastRoleSecondary  = "secondary"
	hastStatusComplete = "complete"
	hastStatusUnknown  = "unknown"
)

// hastVerdict is ADR-0121's own three-way reading of one node's live
// HAST observation, restated over this package's plain data. The
// decision vocabulary is deliberately not the simulator's
// RecoveryVerdict values, but it means exactly the same thing and is
// reached by exactly the same rules.
type hastVerdict int

const (
	// hastUnread means hastd made no usable statement: the query
	// failed, was never attempted, the status line was absent, hastd
	// reported its own "unknown", or the role is one this build does
	// not recognize. It is "could not check", never a verdict.
	hastUnread hastVerdict = iota

	// hastInSync means a real role (primary or secondary) with status
	// "complete" - the strongest thing a single node's own view can
	// say, and still only a point-in-time fact about that node's
	// hastd worker.
	hastInSync

	// hastOutOfSync means a positive statement that the resource is not
	// usable on that node: role "init" (never initialized there), or a
	// real role with a real status other than "complete" (e.g.
	// "degraded"). This is hastd reporting trouble.
	hastOutOfSync
)

// readHAST is ADR-0121's own classification, applied to a
// HASTObservation rather than to a cluster.ReplicaSyncObservation, and
// ordered exactly as internal/cluster/simulate.go's replicaInSync
// orders it:
//
//   - role "init" is checked first, because hastd's "never initialized
//     on this node" is a positive statement that survives the absence
//     of a status line;
//   - an absent status, or hastd's own "unknown", is silence - "we
//     could not check", never "we checked and it is broken";
//   - a real role with status "complete" is in sync;
//   - a real role with any other real status is out of sync - hastd
//     reporting trouble;
//   - anything else, including a role this build does not recognize,
//     is silence again: a new FreeBSD role is Apiary not understanding
//     hastd, which is not evidence of a confirmed outage.
//
// Only a positive statement counts as evidence of badness. That
// distinction is reachable, not theoretical, and inverting it is
// exactly what ADR-0121 was written to prevent: the first
// implementation of that ADR put an absent status and an unrecognized
// role into out-of-sync, which would have stated "confirmed NOT usable
// as-is" on the strength of evidence that said only "could not check".
//
// The duplication is deliberate and bounded: this file cannot reach
// internal/cluster's unexported replicaInSync, and internal/cluster is
// not editable here. The convergence owed is to move the one function
// into a package both can import (internal/invariant has no dependency
// on internal/cluster, so the direction is from cluster to here) and
// have replicaInSync delegate. Until that happens, the two must stay
// step for step - this comment is what says so.
func readHAST(o HASTObservation) hastVerdict {
	if !o.Attempted || !o.Observed {
		return hastUnread
	}
	role := strings.TrimSpace(o.Role)
	status := strings.TrimSpace(o.Status)
	switch {
	case strings.EqualFold(role, hastRoleInit):
		return hastOutOfSync
	case status == "" || strings.EqualFold(status, hastStatusUnknown):
		return hastUnread
	case strings.EqualFold(status, hastStatusComplete) && isRealHASTRole(role):
		return hastInSync
	case isRealHASTRole(role):
		return hastOutOfSync
	default:
		return hastUnread
	}
}

// isRealHASTRole reports whether role is one of the two roles a usable
// HAST resource actually runs in. "init" is handled separately (it is
// a positive statement that the resource was never initialized there),
// and any value a future FreeBSD release might introduce is not
// treated as real.
func isRealHASTRole(role string) bool {
	return strings.EqualFold(role, hastRolePrimary) || strings.EqualFold(role, hastRoleSecondary)
}

// hastRoleClass is what one configured end's own observation
// establishes about whether that end holds the writable primary role.
// The distinction from hastVerdict is deliberate: a real "primary" role
// with a degraded status is still this node's assigned role, and
// reporting it as "not a primary" would understate a real
// dual-primary risk rather than overstate it.
type hastRoleClass int

const (
	// hastRoleUndetermined is silence - no usable statement about this
	// end's role. Never a pass, and never a confirmed non-primary.
	hastRoleUndetermined hastRoleClass = iota
	hastRoleIsPrimary
	hastRoleNotPrimary
)

// classifyHASTRole answers only "does this node say it is the
// writable primary", reusing readHAST so the silence rule is not
// re-decided a second time.
func classifyHASTRole(o HASTObservation) hastRoleClass {
	if readHAST(o) == hastUnread {
		return hastRoleUndetermined
	}
	if strings.EqualFold(strings.TrimSpace(o.Role), hastRolePrimary) {
		return hastRoleIsPrimary
	}
	// Either a real "secondary" role, or "init" - both are positive
	// statements that this end is not the writable primary.
	return hastRoleNotPrimary
}

// HASTPrimarySpec is one replica-backed resource plus the live role
// observation each of its two configured ends reported for it. The two
// ends must be asked independently (GetLocalHASTResourceStatus is
// node-local by design); neither one's view speaks for the other.
type HASTPrimarySpec struct {
	ID, Name, Kind string // Kind: "vm" | "jail"
	OwnerNodeID    string
	ReplicaNodeID  string

	// Owner and Replica are those two ends' own observations. The
	// zero value is honest, not optimistic: Attempted false means no
	// query was possible, which is silence, not a confirmed
	// non-primary role.
	Owner   HASTObservation
	Replica HASTObservation
}

// EvaluateHASTDualPrimary reports one evaluation per replica-backed
// resource: "no HAST resource has two writable primaries" (ADR-0060).
//
// specs carries the per-node role observations the caller gathered. A
// resource named by a spec is decided from that spec's two ends.
// resourceIDs enumerates replica-backed resources for which no
// observation could be gathered at all, and still yields one Unknown
// each with zero-ObservedAt evidence naming the absence - never a
// claim about what this codebase can or cannot see, which is what
// ADR-0060's own disclosure asserted on a premise ADR-0119 has since
// retired. A caller that has not wired observation gathering passes
// its IDs there; the verdict is Unknown either way, but the reason
// stated is the true one.
//
// False requires positive evidence from both ends: each configured end
// reporting the writable primary role. Anything less, in particular
// either end being silent, is Unknown - a missing observation is never
// folded into a pass, which is the warning ADR-0060's Context states
// for this whole catalog.
func EvaluateHASTDualPrimary(resourceIDs []string, specs ...HASTPrimarySpec) []Evaluation {
	evals := make([]Evaluation, 0, len(resourceIDs)+len(specs))
	specByID := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		specByID[spec.ID] = struct{}{}
		evals = append(evals, evaluateHASTDualPrimarySpec(spec))
	}
	for _, id := range resourceIDs {
		if _, covered := specByID[id]; covered {
			continue
		}
		evals = append(evals, Evaluation{
			Name:        "hast-dual-primary",
			Scope:       id,
			Result:      ResultUnknown,
			Explanation: "Cannot confirm this resource has at most one writable HAST primary: no live role observation was gathered for either configured end.",
			Evidence: []Evidence{{
				Source:     "GetLocalHASTResourceStatus (not queried)",
				Detail:     "no HAST role observation was supplied for this resource's owner or replica, so neither end's role is known - silence is never treated as a non-primary role",
				ObservedAt: time.Time{},
			}},
		})
	}
	return evals
}

func evaluateHASTDualPrimarySpec(spec HASTPrimarySpec) Evaluation {
	now := time.Now()
	eval := Evaluation{Name: "hast-dual-primary", Scope: spec.ID}
	eval.Evidence = []Evidence{
		hastEvidence("owner", spec.OwnerNodeID, spec.Owner, now),
		hastEvidence("replica", spec.ReplicaNodeID, spec.Replica, now),
	}

	label := "Resource " + spec.ID
	if spec.Name != "" {
		label += " (" + spec.Name + ", " + spec.Kind + ")"
	}

	switch {
	case spec.OwnerNodeID == "" || spec.ReplicaNodeID == "":
		eval.Result = ResultUnknown
		eval.Explanation = label + " names no distinct HAST counterpart (owner " + orNone(spec.OwnerNodeID) + ", replica " + orNone(spec.ReplicaNodeID) + "), so there is no pair of ends to check for a dual primary."
	case spec.OwnerNodeID == spec.ReplicaNodeID:
		// The same node's single view would otherwise satisfy both
		// ends of the check, which is not evidence of anything.
		eval.Result = ResultUnknown
		eval.Explanation = label + " is configured with " + spec.OwnerNodeID + " as BOTH its HAST owner and replica, so the pair cannot be checked for a dual primary - this is a placement error to correct, not a passing check."
	default:
		owner := classifyHASTRole(spec.Owner)
		replica := classifyHASTRole(spec.Replica)
		switch {
		case owner == hastRoleUndetermined || replica == hastRoleUndetermined:
			eval.Result = ResultUnknown
			eval.Explanation = label + ": could not rule out two writable HAST primaries, because at least one configured end did not report a usable role (owner " + spec.OwnerNodeID + ", replica " + spec.ReplicaNodeID + ")."
		case owner == hastRoleIsPrimary && replica == hastRoleIsPrimary:
			eval.Result = ResultFalse
			eval.Explanation = label + " has TWO writable HAST primaries: " + spec.OwnerNodeID + " and " + spec.ReplicaNodeID + " each report the primary role for it. Writes are not fenced to one end."
		case owner == hastRoleIsPrimary || replica == hastRoleIsPrimary:
			eval.Result = ResultTrue
			eval.Explanation = label + " has exactly one writable HAST primary, reported by " + primaryHolder(owner, replica, spec.OwnerNodeID, spec.ReplicaNodeID) + ". This is a point-in-time observation of both ends' own hastd, not a failover guarantee."
		default:
			eval.Result = ResultTrue
			eval.Explanation = label + " has no dual primary, but neither end reports a writable primary at all (" + roleSummary(spec.Owner) + " on " + spec.OwnerNodeID + ", " + roleSummary(spec.Replica) + " on " + spec.ReplicaNodeID + "). That is not a healthy HAST pair either - see the evidence."
		}
	}
	return eval
}

// primaryHolder names the one end that reported the writable primary
// role, given that exactly one of the two did.
func primaryHolder(owner, replica hastRoleClass, ownerNodeID, replicaNodeID string) string {
	if owner == hastRoleIsPrimary {
		return ownerNodeID
	}
	return replicaNodeID
}

func orNone(nodeID string) string {
	if nodeID == "" {
		return "none"
	}
	return nodeID
}

func roleSummary(o HASTObservation) string {
	role := strings.TrimSpace(o.Role)
	if role == "" {
		role = "no role reported"
	}
	return "role " + role
}

// hastEvidence renders one end's observation. Its ObservedAt is zero
// only when no query was made at all, matching this package's
// convention that a zero ObservedAt means "never runtime-observed at
// all", not "observed a while ago".
func hastEvidence(end, nodeID string, o HASTObservation, now time.Time) Evidence {
	source := "GetLocalHASTResourceStatus for " + end + " " + orNone(nodeID)
	switch {
	case !o.Attempted:
		return Evidence{
			Source:     source,
			Detail:     "no query was made: " + nonEmpty(o.Detail, "this node could not be asked at all (no peer forwarding, no address, or unknown node identity)"),
			ObservedAt: time.Time{},
		}
	case !o.Observed:
		return Evidence{
			Source:     source,
			Detail:     "queried, no usable answer: " + nonEmpty(o.Detail, "no reason reported"),
			ObservedAt: now,
		}
	default:
		return Evidence{
			Source:     source,
			Detail:     "hastd reported role " + quoted(strings.TrimSpace(o.Role)) + ", status " + quoted(strings.TrimSpace(o.Status)) + describeReplication(o.Replication) + " - verbatim, not interpreted",
			ObservedAt: now,
		}
	}
}

func quoted(s string) string {
	if s == "" {
		return `"" (absent)`
	}
	return `"` + s + `"`
}

func describeReplication(replication string) string {
	rep := strings.TrimSpace(replication)
	if rep == "" {
		return ""
	}
	return ", replication " + quoted(rep)
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// replicaSyncEvidence renders one ResourceFact's configured
// replica's own observation for cell-recoverability's sync half, in
// the same shape hastEvidence produces and with the same
// zero-ObservedAt meaning.
func replicaSyncEvidence(f ResourceFact, now time.Time) Evidence {
	return hastEvidence("replica", f.ReplicaNodeID, f.ReplicaSync, now)
}
