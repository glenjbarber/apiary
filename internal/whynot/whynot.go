// Package whynot implements the Why Not Engine v1 (ADR-0061, CODEX.md's
// "Why Not Engine"): a read-only, deterministic answer to concrete
// operator questions ("why can this cell not migrate?", "why is this
// hive unsafe to reboot?", "why is this cell not recoverable?", "why
// can this network not provide connectivity?"), returning the smallest
// actual blocker set - not a dump of every warning - with the evidence
// and invariant behind each conclusion.
//
// Like internal/health/internal/recovery/internal/invariant, this
// package is pure computation with no I/O: internal/frontend gathers
// the raw facts (mostly by calling RPCs/helpers that already exist for
// other pages - the Dependency Graph Simulator, the operational
// invariant catalog, MigrateVM/MigrateJail's own real precondition) and
// calls the Answer* functions below. This package deliberately does
// NOT import internal/cluster or internal/manager: both pull in heavy
// OS-exec dependencies (internal/zfs/internal/jail/internal/bhyve/
// internal/hast) that no pure leaf package or internal/frontend has
// ever been allowed to import in this codebase - the established
// convention at exactly this boundary is small, deliberate duplication
// (see QuorumFact/OwnedResourceFact/ReplicaBackedFact below), not a
// shared helper reaching across it.
//
// This is only ever the "continuously evaluate and report" half -
// CODEX's own text says recommendations "may become Flight Plans, but
// the engine itself is read-only," and no Flight Plan execution engine
// exists in Apiary yet.
package whynot

import (
	"time"

	"github.com/glenjbarber/apiary/internal/invariant"
)

// Verdict is this package's own three-state vocabulary - never a bare
// "safe" bool. VerdictClear means "no blocker found within v1's
// observed scope," never "guaranteed safe."
type Verdict string

const (
	VerdictBlocked Verdict = "blocked"
	VerdictUnknown Verdict = "unknown"
	VerdictClear   Verdict = "clear"
)

// Blocker is one concrete, cited reason contributing to
// Verdict == VerdictBlocked. Invariant names the specific already-
// shipped mechanism this cites - the underlying invariant's own Name
// ("cell-recoverability", "network-route-dns") where one exists, or a
// stable local id ("migrate-replica-precondition",
// "dependency-graph-simulator", "quorum-tolerance") where the citation
// is a different already-shipped mechanism - so a caller can inspect
// or link to the source of a conclusion, not just read prose.
type Blocker struct {
	Invariant string
	Detail    string
	Evidence  []invariant.Evidence // reused directly, never re-invented
}

// Remedy is populated ONLY for a real, code-verified precondition - v1
// has exactly one (AnswerCellMigrate's "set replica_node_id first").
// Every other suggestion (add a raft voter, restore reachability to an
// unreachable voter, configure a replica for an unprotected resource)
// stays as plain Blocker.Detail prose, never a manufactured
// Remedy{Proven: false} that could read as more actionable than it is.
type Remedy struct {
	Detail string
	Proven bool
}

// Answer is one question's full, deterministic result.
type Answer struct {
	Question string // "cell-migrate" | "cell-recoverable" | "hive-reboot" | "network-connectivity"
	Scope    string // the cell/hive/network ID asked about
	Verdict  Verdict
	Blockers []Blocker
	Remedies []Remedy

	// Caveats are permanent, disclosed v1 limitations (live HAST sync
	// status has no RPC exposure, DNS has no observability anywhere in
	// this codebase) passed through verbatim from internal/invariant's
	// own zero-ObservedAt Evidence entries - never restated as new
	// prose, and never counted toward Verdict == VerdictBlocked.
	Caveats []invariant.Evidence
}

// CellFact is the minimal shape internal/frontend gathers once, via a
// single-target GetVM/GetJail lookup plus at most one bounded
// HostStats call - never a cluster-wide fan-out - and hands to both
// AnswerCellMigrate and AnswerCellRecoverable.
type CellFact struct {
	ID, Name, Kind        string // Kind: "vm" | "jail"
	DesiredState          string
	NodeID, ReplicaNodeID string

	// DestinationCapable/DestinationCapableDetail mirror
	// invariant.ResourceFact's own fields exactly: ResultUnknown for a
	// jail (no capability signal exists for jails anywhere in this
	// codebase), an unfetched target, or a failed/timed-out fetch.
	DestinationCapable       invariant.Result
	DestinationCapableDetail string
}

func cellLabel(f CellFact) string {
	return f.Name + " (" + f.Kind + ")"
}

// AnswerCellMigrate re-derives MigrateVM/MigrateJail's own real,
// already-enforced precondition (internal/manager/server.go's MigrateVM
// ~line 778, MigrateJail ~line 1444) directly, rather than delegating
// to EvaluateCellRecoverability: real MigrateVM/MigrateJail never check
// destination capability at all, only that desired_state isn't
// DELETING and that a replica_node_id is already configured (the only
// legal migration target - MigrateVM/MigrateJail reject any other
// target_node_id outright, so there is no separate "which node" input
// here). A cell can be migratable-by-real-code while
// AnswerCellRecoverable reports it Blocked, and vice versa (a fully
// unprotected cell is a real migrate-blocker that
// EvaluateCellRecoverability's own design treats as out of scope) -
// this is a real, disclosed divergence, not a bug in either function.
//
// If this precondition's real logic in MigrateVM/MigrateJail ever
// changes, this remedy text must be updated to match - see that
// function's own doc comment, which cross-references this one.
func AnswerCellMigrate(f CellFact) Answer {
	ans := Answer{Question: "cell-migrate", Scope: f.ID}

	switch {
	case f.DesiredState == "deleting":
		ans.Verdict = VerdictBlocked
		ans.Blockers = []Blocker{{
			Invariant: "migrate-replica-precondition",
			Detail:    cellLabel(f) + " is marked for deletion and cannot migrate.",
		}}
	case f.ReplicaNodeID == "":
		ans.Verdict = VerdictBlocked
		ans.Blockers = []Blocker{{
			Invariant: "migrate-replica-precondition",
			Detail:    cellLabel(f) + " has no replica_node_id configured - MigrateVM/MigrateJail only ever allow a target that is already the cell's configured replica, so there is no legal migration target yet.",
		}}
		ans.Remedies = []Remedy{{
			Proven: true,
			Detail: "Set replica_node_id via UpdateVM/UpdateJail, wait for hastctl to report status: complete on that target, then migrate - this is the exact precondition MigrateVM/MigrateJail already enforce.",
		}}
	default:
		// A replica IS configured, and MigrateVM/MigrateJail have no
		// further precondition to check - real-code migration is not
		// blocked. But live HAST sync status has zero RPC exposure
		// anywhere in this codebase (the same gap
		// EvaluateCellRecoverability/EvaluateHASTDualPrimary both
		// name), so a confident "yes, safe to migrate now" is never
		// achievable in v1 either.
		ans.Verdict = VerdictUnknown
		ans.Caveats = []invariant.Evidence{{
			Source:     "internal/hast (no cluster-wide sync-status RPC)",
			Detail:     "A replica_node_id is configured, satisfying MigrateVM/MigrateJail's own precondition, but Apiary cannot confirm hastctl reports status: complete on the target before migrating - confirm this by hand.",
			ObservedAt: time.Time{},
		}}
	}

	return ans
}

// AnswerCellRecoverable synthesizes the "no replica configured" blocker
// itself for a cell with no ReplicaNodeID: EvaluateCellRecoverability's
// own design treats an unprotected resource as out of its scope (not a
// violation of it - ADR-0060), so filtering that invariant's output
// would silently return nothing for exactly this case. Every other
// case delegates to EvaluateCellRecoverability, which can only ever
// resolve False (destination confirmed incapable) or Unknown (capable
// or unconfirmed, but "synchronized replica" is never confirmable
// either way in v1) - never True.
func AnswerCellRecoverable(f CellFact) Answer {
	ans := Answer{Question: "cell-recoverable", Scope: f.ID}

	if f.ReplicaNodeID == "" {
		ans.Verdict = VerdictBlocked
		ans.Blockers = []Blocker{{
			Invariant: "cell-recoverability",
			Detail:    cellLabel(f) + " has no replica_node_id configured - cell-recoverability's own invariant scope excludes unprotected resources entirely (they are out of scope, not evaluated as a violation), so this blocker is synthesized here rather than cited from that invariant's own output.",
		}}
		return ans
	}

	eval := invariant.EvaluateCellRecoverability([]invariant.ResourceFact{{
		ID: f.ID, Name: f.Name, Kind: f.Kind, ReplicaNodeID: f.ReplicaNodeID,
		DestinationCapable: f.DestinationCapable, DestinationCapableDetail: f.DestinationCapableDetail,
	}})[0]

	for _, ev := range eval.Evidence {
		if ev.ObservedAt.IsZero() {
			ans.Caveats = append(ans.Caveats, ev)
		}
	}

	switch eval.Result {
	case invariant.ResultFalse:
		ans.Verdict = VerdictBlocked
		var blockerEvidence []invariant.Evidence
		for _, ev := range eval.Evidence {
			if !ev.ObservedAt.IsZero() {
				blockerEvidence = append(blockerEvidence, ev)
			}
		}
		ans.Blockers = []Blocker{{Invariant: eval.Name, Detail: eval.Explanation, Evidence: blockerEvidence}}
	default: // ResultUnknown - EvaluateCellRecoverability never returns ResultTrue
		ans.Verdict = VerdictUnknown
	}

	return ans
}

// QuorumFact/OwnedResourceFact/ReplicaBackedFact are small, deliberate
// duplicates of internal/cluster.QuorumImpact/OwnedResourceImpact/
// ReplicaBackedImpact's shapes (ADR-0052) - this package must not
// import internal/cluster (see the package doc comment). internal/
// frontend builds these directly from the rpcpb.SimulateNodeFailureResponse
// it already fetches via the existing SimulateNodeFailure RPC.
type QuorumFact struct {
	Survives bool
	Note     string
}

type OwnedResourceFact struct {
	ID, Name, Kind string
	ReplicaNodeID  string
	Verdict        string // "unprotected" | "unverified_replica"
	Explanation    string
}

type ReplicaBackedFact struct {
	ID, Name, Kind string
	OwnerNodeID    string
	Explanation    string
}

// AnswerHiveReboot reframes the Dependency Graph Simulator's existing
// "what happens if this node disappears right now" report as a "why is
// this hive unsafe to reboot" answer - a reboot is a temporary
// disappearance, and the same quorum/owned-resource consequences apply
// during it. Quorum-not-surviving and EVERY owned resource with
// Verdict "unprotected" or "unverified_replica" are Blockers with equal
// severity - an unverified replica is not proven to protect anything,
// so treating it as a softer finding than "no replica at all" would be
// exactly the "missing observation treated as a passed safety check"
// CODEX's own text warns against. A ReplicaBackedFact (this node backs
// a DIFFERENT node's resource as its HAST replica) never appears in
// Blockers - that resource keeps running, unaffected, on its real
// owner; only its redundancy is lost during this reboot, surfaced as a
// Caveat only.
func AnswerHiveReboot(nodeID string, quorum QuorumFact, owned []OwnedResourceFact, replicaBacked []ReplicaBackedFact) Answer {
	ans := Answer{Question: "hive-reboot", Scope: nodeID}

	if !quorum.Survives {
		ans.Blockers = append(ans.Blockers, Blocker{
			Invariant: "quorum-tolerance",
			Detail:    "Raft quorum would not survive this hive's loss. " + quorum.Note,
		})
	}

	for _, o := range owned {
		switch o.Verdict {
		case "unprotected":
			ans.Blockers = append(ans.Blockers, Blocker{
				Invariant: "dependency-graph-simulator",
				Detail:    o.Name + " (" + o.Kind + ") has no replica configured - it will stop with no failover for the duration of this reboot. " + o.Explanation,
			})
		case "unverified_replica":
			ans.Blockers = append(ans.Blockers, Blocker{
				Invariant: "dependency-graph-simulator",
				Detail:    o.Name + " (" + o.Kind + ") has a replica configured, but live HAST sync status can never be confirmed in v1 - do not treat it as protected without a manual hastctl status check before rebooting. " + o.Explanation,
			})
		}
	}

	for _, rb := range replicaBacked {
		ans.Caveats = append(ans.Caveats, invariant.Evidence{
			Source: "dependency-graph-simulator (replica-backed resource)",
			Detail: rb.Name + " (" + rb.Kind + ") stays running, unaffected, on its real owner " + rb.OwnerNodeID + " during this reboot - it only loses this hive as a HAST replica target until reconfigured elsewhere. " + rb.Explanation,
		})
	}

	if len(ans.Blockers) > 0 {
		ans.Verdict = VerdictBlocked
	} else {
		ans.Verdict = VerdictClear
	}

	return ans
}

// AnswerNetworkConnectivity takes the RAW invariant.NetworkFact (built
// before EvaluateNetworkRoute folds it into an Evaluation) because that
// folded Evidence list cannot be reliably split into "actual blocker"
// vs. "permanent disclosed caveat" from outside the package without
// parsing prose. Blockers come only from a BridgeObservation with
// Status == "down" - a real, confirmed problem on a node hosting a
// resource on this network. A fetch failure/error is cited as a
// Caveat, not a Blocker: it means the route could not be confirmed, not
// that it is confirmed broken. The permanent DNS-observability gap is
// always a Caveat. This can never resolve VerdictClear, mirroring
// EvaluateNetworkRoute's own "never better than Unknown" ceiling - DNS
// is never verifiable in v1 regardless of route health.
func AnswerNetworkConnectivity(fact invariant.NetworkFact) Answer {
	ans := Answer{Question: "network-connectivity", Scope: fact.ID}
	now := time.Now()

	for _, obs := range fact.Observations {
		switch {
		case obs.Status == "down":
			ans.Blockers = append(ans.Blockers, Blocker{
				Invariant: "network-route-dns",
				Detail:    fact.Name + "'s bridge is reported down on node " + obs.NodeID + ".",
				Evidence:  []invariant.Evidence{{Source: "GetLocalNetworkBridgeStatus on " + obs.NodeID, Detail: "bridge reported down", ObservedAt: now}},
			})
		case obs.Err != "":
			ans.Caveats = append(ans.Caveats, invariant.Evidence{
				Source: "GetLocalNetworkBridgeStatus on " + obs.NodeID,
				Detail: "fetch failed: " + obs.Err + " - route could not be confirmed on this node, not confirmed broken",
			})
		case obs.Status != "up":
			ans.Caveats = append(ans.Caveats, invariant.Evidence{
				Source: "GetLocalNetworkBridgeStatus on " + obs.NodeID,
				Detail: "bridge status unreported/unknown on this node",
			})
		}
	}

	ans.Caveats = append(ans.Caveats, invariant.Evidence{
		Source:     "DNS (no observability)",
		Detail:     "Apiary does not expose a guest DHCP DNS option or resolver result through RPC - DNS path is always unknown in v1, regardless of route health.",
		ObservedAt: time.Time{},
	})

	switch {
	case len(ans.Blockers) > 0:
		ans.Verdict = VerdictBlocked
	default:
		ans.Verdict = VerdictUnknown
	}

	return ans
}
