package hostpkg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Action is what a plan proposes to do to one package. The vocabulary
// covers removal and reinstall because an operator's question is "what
// happens to my host", and a plan that cannot even name those outcomes
// forces them to reason about pkg's own behaviour instead of this
// build's. Apply's refusal to perform a removal is a separate,
// explicitly-enforced boundary (ErrRemovalUnsupported) - the plan is
// allowed to describe it so the operator can see it.
type Action string

const (
	// ActionNone means the package needs no change.
	ActionNone Action = "none"

	// ActionUpgrade means a newer version is available and would be
	// installed.
	ActionUpgrade Action = "upgrade"

	// ActionReinstall means the same version would be reinstalled -
	// used when a caller-supplied policy marks a package as needing a
	// clean reinstall (a patched package rebuilt against different
	// options, say). A reinstall is never inferred from pkg's output in
	// v1; it requires the caller to say so explicitly.
	ActionReinstall Action = "reinstall"

	// ActionRemove means the package would be removed. v1 never proposes
	// this on its own evidence, and v1 never performs it.
	ActionRemove Action = "remove"
)

// RestartRequirement is the three-state answer to "will this need a
// restart?". It is a three-state type for the same reason UpdateStatus
// is: "no restart needed" and "we do not know" are completely
// different things to tell an operator before they touch a host that
// runs HAST and ZFS, and pkg genuinely does not answer this question
// for us.
type RestartRequirement string

const (
	// RestartNone means this build positively determined no restart is
	// needed. It is only produced from an explicit policy decision, not
	// from an absence of evidence.
	RestartNone RestartRequirement = "none"

	// RestartRequired means this build positively determined a restart
	// or reboot is needed.
	RestartRequired RestartRequirement = "required"

	// RestartUnknown means it could not be determined. This is the
	// default for every package not named in the caller's policy: pkg
	// does not declare which of its packages need a service restart,
	// and guessing "no" is the unsafe direction.
	RestartUnknown RestartRequirement = "unknown"
)

// Change is one package's entry in a plan.
type Change struct {
	Package InstalledPackage
	Action  Action

	// TargetVersion is the version the action would install. For
	// ActionUpgrade it is Package.Candidate; for ActionReinstall it is
	// Package.Version; for ActionRemove it is empty.
	TargetVersion string

	// Reason is the operator-readable justification, quoting the
	// evidence the action is based on. It is never empty.
	Reason string

	// Restart is this change's restart requirement. See
	// RestartRequirement for why it is three-state.
	Restart RestartRequirement
}

// BaseAction is what the plan proposes for the base system itself.
type BaseAction string

const (
	// BaseActionNone means no base change is proposed.
	BaseActionNone BaseAction = "none"

	// BaseActionUpdate means a base system update is available and
	// would be installed.
	BaseActionUpdate BaseAction = "update"

	// BaseActionUnknown means the base system's update state could not
	// be determined, so no action can be proposed. It is deliberately
	// not folded into BaseActionNone: "nothing to do" and "we cannot
	// tell" must not share a value.
	BaseActionUnknown BaseAction = "unknown"
)

// Hazard names a host condition that makes a package update risky in a
// way this package will not decide on the operator's behalf. See
// Plan.Safety.
type Hazard string

const (
	// HazardHAST means this host is a live HAST primary or secondary.
	// Replacing a running HAST provider's files underneath hastd is a
	// genuine way to break replication, and to lose a voter's disk.
	HazardHAST Hazard = "hast"

	// HazardZFS means this host has ZFS pools imported.
	HazardZFS Hazard = "zfs"

	// HazardRaftVoter means this host is a raft voter, so an update
	// that restarts a daemon on it touches consensus availability.
	HazardRaftVoter Hazard = "raft_voter"
)

// Gate is how heavily the plan's own hazard findings should gate the
// confirmation phrase an operator has to type.
type Gate string

const (
	// GateClear means no hazard was found. The standard confirmation
	// phrase applies.
	GateClear Gate = "clear"

	// GateCaution means at least one hazard was found. A different,
	// more alarming phrase applies, and it is spelled out on the page -
	// typing the ordinary phrase does not work.
	GateCaution Gate = "caution"
)

// ConfirmPhrase is the phrase required when a plan is not gated.
const ConfirmPhrase = "yes-update-packages"

// ConfirmHazardPhrase is the phrase required when a plan carries at
// least one hazard. It is a different string from ConfirmPhrase on
// purpose: an operator who rehearsed the confirmation flow on a safe
// host must not be able to muscle through a HAST primary by muscle
// memory.
const ConfirmHazardPhrase = "yes-update-packages-on-a-hazardous-host"

// HostRisk is the caller-supplied picture of what this host is actually
// doing, which this package cannot determine for itself.
//
// It is supplied rather than shelled out for a specific reason: HAST
// and ZFS state already have their own packages and their own
// observations in this codebase (internal/hast, internal/zfs), and a
// second, differently-shaped source of truth for the same facts would
// eventually disagree with the first. A caller that has already read
// those packages passes what it knows here; a caller that has not
// passes what it does not know in Unknown, and the plan degrades to a
// caution gate rather than to a clean one.
type HostRisk struct {
	// HASTRole is "primary", "secondary", "init", or "" when this host
	// runs no HAST resource at all.
	HASTRole string

	// HASTResources names the HAST resources this host holds, for
	// display only.
	HASTResources []string

	// ZFSImportedPools names this host's imported ZFS pools, for
	// display only.
	ZFSImportedPools []string

	// RaftVoter is true when this host votes in raft consensus.
	RaftVoter bool

	// Unknowns names facts the caller could not establish. Any entry
	// forces a caution gate: an unknown HAST role is not a known-safe
	// HAST role.
	Unknowns []string
}

// Safety is the plan's hazard assessment. It never blocks on its own in
// v1 - deciding that a HAST host may never be updated is an operational
// policy question, not a code question - but it does change the phrase
// required, so it cannot be ignored by accident.
type Safety struct {
	Gate    Gate
	Hazards []Hazard
	Notes   []string
}

// Plan is a preview of what an update would do. It is a value, computed
// once from a specific Inventory, and it carries that inventory's
// fingerprint so Apply can prove the world has not changed underneath
// it.
type Plan struct {
	// CreatedAt is when the plan was computed.
	CreatedAt time.Time

	// InventoryFingerprint is the Inventory.Fingerprint this plan was
	// computed from. Apply recomputes it from a fresh read and refuses
	// to act if it differs.
	InventoryFingerprint string

	// Base is what would happen to the base system.
	Base BaseAction

	// BaseReason explains Base, quoting the observed evidence.
	BaseReason string

	// RebootRequired is true when a base system update is proposed.
	// This is a definitive statement, not a guess: FreeBSD's own
	// documented procedure for a base update requires a reboot, so
	// unlike per-package restarts it is not policy-dependent.
	RebootRequired bool

	// Changes is every package that would change, sorted by name.
	Changes []Change

	// Safety is the hazard assessment for this host.
	Safety Safety

	// Warnings are additional operator-facing cautions, including
	// anything v1 cannot do and wants the operator to know about before
	// finding out afterwards.
	Warnings []string
}

// ReinstallPolicy names packages the caller explicitly wants rebuilt at
// their current version. v1 never infers a reinstall, and a request for
// a package that also has an update available is resolved as an upgrade
// instead.
type ReinstallPolicy map[string]string // package name -> reason

// RestartPolicy names packages whose upgrade is known to require a
// service restart afterwards, and why. A package absent from this map
// is RestartUnknown, never RestartNone.
type RestartPolicy map[string]string // package name -> reason

// PlanOptions configures PlanFrom.
type PlanOptions struct {
	// Reinstall names packages to reinstall at their current version.
	Reinstall ReinstallPolicy

	// Restart names packages whose upgrade requires a restart.
	Restart RestartPolicy
}

// PlanFrom computes a pure preview of what an update would do. It reads
// nothing, runs nothing, and changes nothing: everything it reports is
// derived from inv, which must be a real inventory from Collect.
func PlanFrom(inv Inventory, risk HostRisk, opts PlanOptions) Plan {
	plan := Plan{
		CreatedAt:            inv.ObservedAt,
		InventoryFingerprint: inv.Fingerprint(),
		Changes:              make([]Change, 0, len(inv.Ports)),
	}

	switch inv.Base.UpdateStatus {
	case UpdateStatusOutdated:
		plan.Base = BaseActionUpdate
		plan.RebootRequired = true
		plan.BaseReason = "pkg reports a base system update is available: " + inv.Base.Detail
	case UpdateStatusUnknown:
		plan.Base = BaseActionUnknown
		plan.BaseReason = "the base system's update state could not be determined: " + inv.Base.Detail
	default:
		plan.Base = BaseActionNone
		plan.BaseReason = "no base system update is available"
	}

	for _, p := range inv.Ports {
		change := Change{Package: p, Action: ActionNone, TargetVersion: p.Version}
		reason, wantsReinstall := opts.Reinstall[p.Name]
		switch {
		case p.UpdateStatus == UpdateStatusOutdated:
			// An upgrade always wins over a reinstall request for the
			// same package: reinstalling the installed version when a
			// newer one exists would satisfy neither the reinstall
			// request's intent nor the inventory's evidence.
			change.Action = ActionUpgrade
			change.TargetVersion = p.Candidate
			change.Reason = fmt.Sprintf("%s-%s installed, %s available", p.Name, p.Version, p.Candidate)
		case wantsReinstall:
			change.Action = ActionReinstall
			change.Reason = reason
		case p.UpdateStatus == UpdateStatusUnknown:
			// An unknown package is not a no-op. It becomes an entry
			// with ActionNone and an explicit reason, so it is visible
			// in the plan as a question that was not answered rather
			// than silently absent from the list.
			change.Action = ActionNone
			change.TargetVersion = ""
			change.Reason = "update status unknown: " + p.Detail
		default:
			change.Reason = p.Name + "-" + p.Version + " is current"
		}
		change.Restart = restartFor(change, opts.Restart)
		plan.Changes = append(plan.Changes, change)
	}
	sort.Slice(plan.Changes, func(i, j int) bool { return plan.Changes[i].Package.Name < plan.Changes[j].Package.Name })

	plan.Safety = assessSafety(risk)
	plan.Warnings = planWarnings(inv, plan)
	return plan
}

// restartFor resolves one change's restart requirement. The default is
// RestartUnknown, never RestartNone: see the type comment.
func restartFor(change Change, policy RestartPolicy) RestartRequirement {
	if change.Action == ActionNone {
		// Nothing changes, so nothing restarts. This is a deduction
		// from a positively-determined no-op, not from silence.
		return RestartNone
	}
	if reason, ok := policy[change.Package.Name]; ok && reason != "" {
		return RestartRequired
	}
	return RestartUnknown
}

// assessSafety turns the caller-supplied HostRisk into a gate. An
// unestablished fact forces caution, because "we could not check" must
// never read as "we checked and it is fine" - the same rule
// internal/cluster/simulate.go applies to replica reachability.
func assessSafety(risk HostRisk) Safety {
	safety := Safety{Gate: GateClear}
	hasHAST := strings.EqualFold(risk.HASTRole, "primary") || strings.EqualFold(risk.HASTRole, "secondary")
	switch {
	case hasHAST:
		safety.Hazards = append(safety.Hazards, HazardHAST)
		safety.Notes = append(safety.Notes, fmt.Sprintf(
			"this Comb is a HAST %s holding %s - upgrading packages on it replaces files a live hastd is using, and a failed HAST operation here can take a replica out of service",
			risk.HASTRole, describeList(risk.HASTResources, "no named resources")))
	case strings.EqualFold(risk.HASTRole, "init") || risk.HASTRole != "":
		safety.Notes = append(safety.Notes, fmt.Sprintf(
			"this Comb reports HAST role %q, which this build does not recognise as a real role, so its HAST exposure is unestablished rather than absent",
			risk.HASTRole))
	}
	if len(risk.ZFSImportedPools) > 0 {
		safety.Hazards = append(safety.Hazards, HazardZFS)
		safety.Notes = append(safety.Notes, fmt.Sprintf(
			"this Comb has %s imported - a base system update requires a reboot, and a reboot on a pool-holding voter is a quorum event",
			describeList(risk.ZFSImportedPools, "no named pools")))
	}
	if risk.RaftVoter {
		safety.Hazards = append(safety.Hazards, HazardRaftVoter)
		safety.Notes = append(safety.Notes, "this Comb is a raft voter - any package update that restarts a daemon here affects consensus availability")
	}
	for _, unknown := range risk.Unknowns {
		safety.Notes = append(safety.Notes, "unestablished: "+unknown)
	}
	if len(safety.Hazards) > 0 || len(risk.Unknowns) > 0 {
		safety.Gate = GateCaution
	}
	return safety
}

// planWarnings collects the things an operator should know before
// applying, including v1's own limits.
func planWarnings(inv Inventory, plan Plan) []string {
	warnings := make([]string, 0, 5)
	if plan.RebootRequired && len(plan.Safety.Hazards) == 0 && len(plan.Safety.Notes) == 0 {
		// A reboot on a host with no known storage or consensus role is
		// still a reboot, and worth a note even when it is not a
		// hazard.
		warnings = append(warnings, "a base system update requires this Comb to reboot before the new kernel is in effect")
	}
	if len(inv.Unknown) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d thing(s) about this host's package state could not be determined; the plan below covers only what was positively observed",
			len(inv.Unknown)))
	}
	if plan.Base == BaseActionUnknown {
		warnings = append(warnings, "the base system is not covered by this plan because its update state could not be determined")
	}
	unknownRestarts := 0
	for _, change := range plan.Changes {
		if change.Restart == RestartUnknown {
			unknownRestarts++
		}
	}
	if unknownRestarts > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d changed package(s) have an undetermined restart requirement - pkg does not declare this, so check the package's own service before assuming the host is unaffected", unknownRestarts))
	}
	warnings = append(warnings, "this version never removes a package, never runs `pkg update`, and has no scheduled or unattended upgrade path")
	return warnings
}

// RequiredConfirmPhrase is the exact phrase an operator must supply to
// apply this plan. It is a method rather than a constant so the hazard
// gate cannot be forgotten at a call site.
func (p Plan) RequiredConfirmPhrase() string {
	if p.Safety.Gate == GateCaution {
		return ConfirmHazardPhrase
	}
	return ConfirmPhrase
}

// Actionable is the set of package names this plan would actually change
// with an upgrade or a reinstall, sorted. Apply uses exactly this set -
// nothing else can ever reach a command vector.
func (p Plan) Actionable() []string {
	names := make([]string, 0, len(p.Changes))
	for _, change := range p.Changes {
		switch change.Action {
		case ActionUpgrade, ActionReinstall:
			names = append(names, change.Package.Name)
		}
	}
	sort.Strings(names)
	return names
}

// HasRemoval reports whether the plan contains any removal, which v1
// refuses to perform.
func (p Plan) HasRemoval() bool {
	for _, change := range p.Changes {
		if change.Action == ActionRemove {
			return true
		}
	}
	return false
}

// Summary is a one-line, operator-readable description of the plan's
// size. It never claims completeness: the "known" qualifier is
// deliberate, because a plan built on a partial inventory must not read
// as a full account of the host.
func (p Plan) Summary() string {
	upgrades, reinstalls := 0, 0
	for _, change := range p.Changes {
		switch change.Action {
		case ActionUpgrade:
			upgrades++
		case ActionReinstall:
			reinstalls++
		}
	}
	parts := make([]string, 0, 3)
	if upgrades > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", upgrades, plural(upgrades, "upgrade", "upgrades")))
	}
	if reinstalls > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", reinstalls, plural(reinstalls, "reinstall", "reinstalls")))
	}
	switch p.Base {
	case BaseActionUpdate:
		parts = append(parts, "base system update")
	case BaseActionUnknown:
		parts = append(parts, "base system update unknown")
	}
	if len(parts) == 0 {
		return "no known changes"
	}
	out := strings.Join(parts, ", ")
	if p.RebootRequired {
		out += " (requires a reboot)"
	}
	return out
}

// plural is a deliberately tiny helper: one is not a special enough
// case in this package's prose to justify fmt's "d"-suffix trickery, and
// a wrong "1 upgrades" in an operator-facing summary is worth three
// lines of code to avoid.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// digest is a short, stable content hash. It is used for the plan
// fingerprint only - never for authentication or any secret - so sha256
// truncated to 16 hex characters is ample and keeps the fingerprint
// short enough to display and retype.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// describeList renders a list of names for a sentence, naming them
// individually when short and counting them when long.
func describeList(items []string, empty string) string {
	switch {
	case len(items) == 0:
		return empty
	case len(items) <= 4:
		return strings.Join(quoteAll(items), ", ")
	default:
		return fmt.Sprintf("%d of them (%s, and %d more)", len(items), strings.Join(quoteAll(items[:2]), ", "), len(items)-2)
	}
}
