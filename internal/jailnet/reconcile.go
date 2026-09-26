package jailnet

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/glenjbarber/apiary/internal/jail"
	"github.com/glenjbarber/apiary/internal/vlan"
)

// Verdict is what one reconcile pass concluded about one jail's VNET
// networking. It is modelled directly on internal/cluster/simulate.go's
// RecoveryVerdict vocabulary, and for the same reason: the strong
// verdicts have to be earned by observation, and the interesting case
// is the one where the answer could not be earned at all.
//
// The contract every one of these obeys: no Verdict other than
// VerdictUnknown is ever returned on the strength of a failed
// observation. "Could not look" is not "looked and fine" and is not
// "looked and broken".
type Verdict string

const (
	// VerdictInSync means every question this pass asked was answered,
	// and every answer matched what was intended. Nothing was changed.
	VerdictInSync Verdict = "in_sync"

	// VerdictRepaired means drift was found and all of it was repaired
	// in this same pass. This is a success verdict, but deliberately
	// distinguishable from VerdictInSync: a jail that needed its
	// interface rebuilt after every reboot has a real problem, and a UI
	// that renders both as plain green is lying by omission.
	VerdictRepaired Verdict = "repaired"

	// VerdictDrifted means drift was found and could not be repaired
	// from here. The one case that is genuinely not repairable in place
	// is FindingRestartRequired: jail(8) cannot hand a vnet interface
	// to an already-running jail.
	VerdictDrifted Verdict = "drifted"

	// VerdictUnknown means this pass could not determine whether the
	// jail's networking is correct. Not "correct", not "incorrect", not
	// "absent". A node where ifconfig cannot run, or a jls that timed
	// out, lands here, and the right operator response is to fix the
	// node - not to act on a networking conclusion that was never
	// reached.
	VerdictUnknown Verdict = "unknown"

	// VerdictNotRunning means the jail itself is not running, so there
	// was nothing to observe inside it. This is a definite, useful
	// answer - not a failure to look - and it is what the reconciler
	// returns during first-time provisioning, before the jail exists.
	VerdictNotRunning Verdict = "not_running"
)

// Finding names one specific thing that was wrong, in enough detail to
// act on without reading the code. It is a string type rather than an
// enum because the set is open-ended (a specific ifconfig error, a
// specific unexpected state) and an enum would either grow forever or
// flatten the evidence that makes the value worth having.
type Finding string

const (
	// FindingRestartRequired means the jail is running but does not
	// have the interface it is supposed to have. Not repairable in
	// place: jail(8) has no way to add a vnet interface to an
	// already-running jail, and the pair's jail-side end went with it.
	// The only correct repair is to restart the jail, which the
	// reconciler deliberately does not do itself - restarting a running
	// jail is visible and disruptive, and belongs to whoever owns the
	// jail's lifecycle.
	FindingRestartRequired Finding = "restart_required"

	// FindingEpairMissing means there was no usable epair(4) pair
	// recorded for this jail, or the recorded pair is not present on
	// this node. Expected after any reboot - epair(4) pairs are not
	// persistent - and repaired by provisioning a fresh pair.
	FindingEpairMissing Finding = "epair_missing"

	// FindingEpairRecordCorrupt means a record exists but does not name
	// a plausible epair(4) pair. Repaired the same way as
	// FindingEpairMissing, but surfaced separately because it means the
	// bookkeeping file was damaged rather than merely cold.
	FindingEpairRecordCorrupt Finding = "epair_record_corrupt"

	// FindingHostSideDown means the host side exists but is not up, so
	// the pair carries no traffic even though it is attached.
	FindingHostSideDown Finding = "host_side_down"

	// FindingBridgeMembershipWrong means the host side exists but is
	// attached to a different bridge than intended, or to none. This is
	// what a destroyed-and-recreated bridge(4) looks like: the interface
	// survives, the membership does not.
	FindingBridgeMembershipWrong Finding = "bridge_membership_wrong"

	// FindingAddressWrong means the jail's own interface does not carry
	// exactly the assigned address and prefix length.
	FindingAddressWrong Finding = "address_wrong"

	// FindingForeignAddress means the jail's interface carries an
	// address Apiary never assigned it. Repaired by removing it,
	// because a second address means the jail's traffic goes out with
	// an identity nothing in Apiary's state accounts for. Surfaced
	// separately from FindingAddressWrong because it is the observable
	// trace of something else configuring this jail - a DHCP client
	// inside it, a hand-run ifconfig, a second Apiary - which is worth
	// an operator knowing about even though the repair is routine.
	FindingForeignAddress Finding = "foreign_address"

	// FindingRouteWrong means the jail's default route is not the one
	// intended.
	FindingRouteWrong Finding = "route_wrong"
)

// Result is one pass's evidence for one jail. It is a snapshot, not a
// boolean, for the reason this whole package is a snapshot reconciler:
// reconciling means comparing, and a comparison needs both sides.
//
// Observed and Verdict are never in conflict: Observed=false always
// implies VerdictUnknown and VerdictUnknown always implies
// Observed=false. A caller can check either one and get the same
// answer, which is what stops a future caller from checking only
// Verdict == VerdictInSync and ignoring the Observed field.
type Result struct {
	JailID string

	// Bridge, HostSide and JailSide are the epair(4) pair and the
	// bridge as they stand after this pass, which may differ from what
	// was recorded before it.
	Bridge   string
	HostSide string
	JailSide string

	// Observed is true only when every question this pass asked was
	// answered. false means at least one observation failed, and Detail
	// says which. It never means "nothing is wrong".
	Observed bool

	Verdict Verdict

	// Findings are the specific things that were wrong, in the order
	// they were found. Empty on VerdictInSync. On VerdictUnknown it
	// holds only what was established *before* the observation that
	// failed, so a caller must read the absence of a finding on an
	// unknown verdict as "not established" rather than as "fine" - the
	// one thing an unknown verdict must never be read as saying.
	Findings []Finding

	// Detail is human-readable evidence: what was observed, what was
	// repaired, and why a pass could not conclude anything. It is part
	// of the contract, not decoration - a Verdict with no Detail is an
	// answer nobody can act on.
	Detail string

	// Repairs is the ordered list of changes this pass actually made,
	// and only changes that succeeded. It is what makes idempotency
	// checkable from outside: a second pass over a converged jail
	// appends nothing here. A repair that was attempted and failed
	// appears in Detail, never here.
	Repairs []string
}

// Unknown reports whether this pass failed to establish anything, the
// one condition every caller must check before acting on or reporting
// any other field.
func (r Result) Unknown() bool { return r.Verdict == VerdictUnknown }

// errStateUnknown is wrapped by every observation failure, so a caller
// holding only an error can still tell "could not look" from "looked,
// and it is wrong" without string matching. Every Result verdict is
// derived from an error of this shape plus whatever definite answers
// were gathered before it.
var errStateUnknown = errors.New("networking state could not be determined")

// Reconciler brings one jail's VNET networking to its intended state,
// and is the only thing in this package that runs commands.
type Reconciler struct {
	// Bridge drives the host-side interface lifecycle: creating the
	// bridge, adding a member to it, and creating/destroying an epair
	// pair. *vlan.Manager satisfies it unchanged - this package adds
	// nothing to internal/vlan's own interface, it only uses the subset
	// internal/cluster's reconciler already depends on.
	Bridge Bridger

	// Jail is the jail(8)/jexec(8) driver. *jail.Manager satisfies it.
	Jail JailDriver

	// Runner executes host-side ifconfig(8) calls, which vlan.Manager
	// does not expose: "is this epair still a member of the bridge we
	// think it is" is a question only a raw ifconfig can answer, and it
	// is the single most important drift question this package asks.
	// nil means the real shell, which is the only correct setting in
	// production; tests inject a fake.
	Runner Runner

	// StatePath is the node-local epair record file. Empty means
	// DefaultStatePath.
	StatePath string
}

// Bridger is the subset of *vlan.Manager this package needs, declared
// here for the same reason internal/cluster declares its own
// per-dependency interfaces: so this package's logic can be tested
// against a fake, with no FreeBSD anywhere in sight.
type Bridger interface {
	EnsureBridge(ctx context.Context, name string) (created bool, err error)
	// EnsureMember now returns a membership state rather than a bare
	// error (ADR-0138). This package's only use of it is an epair(4)
	// host side, which can never be a Bridge SVI - see the call site.
	EnsureMember(ctx context.Context, bridge, iface string) (vlan.Membership, error)
	EnsureEpair(ctx context.Context, bridge string) (hostSide, jailSide string, err error)
	DestroyEpair(ctx context.Context, hostSide string) error
}

// JailDriver is the subset of *jail.Manager this package needs: the
// in-jail half, observing and configuring the address and default
// route through jexec(8). *jail.Manager satisfies it unchanged.
type JailDriver interface {
	JailExists(ctx context.Context, name string) (bool, error)
	ObserveNet(ctx context.Context, jail, iface string) (jail.JailNetState, error)
	EnsureAddressing(ctx context.Context, jail, iface string, addr jail.Address, gw string) error
}

// defaultRunner is the production Runner, used when Reconciler.Runner
// is nil. The real shell is the only correct setting in production, so
// a nil Runner never degrades into a silent no-op.
func (r *Reconciler) run(ctx context.Context, name string, args ...string) (string, error) {
	if r.Runner == nil {
		return execRunner{}.Run(ctx, name, args...)
	}
	return r.Runner.Run(ctx, name, args...)
}

// Ensure converges one jail's VNET networking, and is the whole of
// this package's behavior. It is safe to call on every reconcile tick.
//
// The order is the same one internal/cluster's ensureJail already uses
// for a VM: network artifact, then interface, then the jail, then the
// jail's own addressing. Each step only runs if the one before it
// succeeded, so a failure never leaves a record claiming something that
// did not happen.
//
// Every early return is a Result, never a bare error. A caller that
// wants to treat "could not look" differently from "looked and broken"
// needs that distinction, and returning it only as an error would force
// every such caller to string-match.
func (r *Reconciler) Ensure(ctx context.Context, jailID string, addr Addressing) Result {
	res := Result{JailID: jailID, Bridge: addr.Bridge, JailSide: addr.Interface}

	switch {
	case jailID == "":
		return unknown(res, "no jail id, so there is nothing to reconcile networking for")
	case r.Jail == nil:
		return unknown(res, "no jail driver is configured on this node, so this jail's networking could not be looked at or changed")
	case r.Bridge == nil:
		return unknown(res, "no bridge driver is configured on this node, so this jail's networking could not be looked at or changed")
	}

	host, err := r.reconcileHost(ctx, jailID, addr)
	res.HostSide = host.hostSide
	res.JailSide = host.jailSide
	res.Bridge = host.bridge
	res.Findings = append(res.Findings, host.findings...)
	res.Repairs = append(res.Repairs, host.repairs...)
	if err != nil {
		return unknown(res, err.Error())
	}

	return r.reconcileJail(ctx, jailID, addr, host, res)
}

// hostOutcome is the host half of one pass, kept separate from Result
// so the jail half can extend the same Result without re-deciding any
// of the host half's findings.
type hostOutcome struct {
	hostSide string
	jailSide string
	bridge   string
	findings []Finding
	repairs  []string
}

// reconcileHost ensures the bridge exists, that an epair(4) pair exists
// and is recorded, that the host side is up, and that the host side is
// a member of the intended bridge. Each of those is observed before it
// is touched, and each repair is itself idempotent, so a pass over a
// converged node issues two `ifconfig` reads and nothing else.
func (r *Reconciler) reconcileHost(ctx context.Context, jailID string, addr Addressing) (hostOutcome, error) {
	var out hostOutcome

	state, err := LoadState(r.StatePath)
	if err != nil {
		// A record file that cannot be read is unknown, not "this jail
		// has no pair". Proceeding without it would create a second
		// pair for a jail that already has one, which is exactly the
		// interface leak the file exists to prevent.
		return out, fmt.Errorf("%w: %s", errStateUnknown, err)
	}

	rec, present, usable := state.Lookup(jailID)
	if present && !usable {
		// A record that names an impossible pair is worse than no
		// record: it would be handed straight to jail(8). Discard it
		// and fall through to the cold-start path below, which
		// provisions a real pair and overwrites the broken entry.
		out.findings = append(out.findings, FindingEpairRecordCorrupt)
		out.repairs = append(out.repairs, fmt.Sprintf("discarded an untrustworthy epair record for jail %q (%+v)", jailID, state.Epairs[jailID]))
		rec = EpairRecord{}
	}
	if rec.HostSide == "" {
		// Nothing usable recorded: provision from scratch.
		return r.provision(ctx, jailID, addr, state, out)
	}

	out.hostSide, out.jailSide, out.bridge = rec.HostSide, rec.JailSide, addr.Bridge
	if rec.Bridge != "" && rec.Bridge != addr.Bridge {
		// A record naming a different bridge is a jail that moved
		// networks. The interface is not merely wrongly attached now,
		// it belongs somewhere else entirely - but it is the same
		// repair either way, so it is handled as one case below and
		// recorded here so the operator can see why.
		out.findings = append(out.findings, FindingBridgeMembershipWrong)
	}

	host, err := r.ObserveHost(ctx, rec.HostSide)
	if err != nil {
		return out, err
	}
	if !host.Exists {
		// The recorded pair is not on this node. This is the normal
		// state after any reboot, because epair(4) pairs are not
		// persistent and nothing recreates them at boot. The old record
		// is replaced rather than reused, because its names are
		// permanently dead here and handing jail(8) one of them would
		// fail the create on every future tick.
		out.findings = append(out.findings, FindingEpairMissing)
		return r.provision(ctx, jailID, addr, state, out, fmt.Sprintf(
			"recorded epair %s is not present on this node (epair(4) pairs do not survive a reboot)", rec.HostSide))
	}

	if !host.Up {
		if _, err := r.run(ctx, "ifconfig", rec.HostSide, "up"); err != nil {
			return out, fmt.Errorf("%w: bringing host interface %s up: %s", errStateUnknown, rec.HostSide, err)
		}
		out.findings = append(out.findings, FindingHostSideDown)
		out.repairs = append(out.repairs, fmt.Sprintf("brought host interface %s up", rec.HostSide))
	}

	if host.MemberOf != addr.Bridge {
		// Leaving a member attached to the wrong bridge is not "mostly
		// fine": it puts the jail's traffic on another network's
		// segment, which is the one thing a dedicated jail network is
		// supposed to prevent. Remove before adding, because a bridge
		// membership is exclusive - adding without removing is what
		// leaves a jail silently reachable from a network it was never
		// attached to.
		if host.MemberOf != "" {
			if _, err := r.run(ctx, "ifconfig", host.MemberOf, "deletem", rec.HostSide); err != nil && !isAbsent(err) {
				return out, fmt.Errorf("%w: removing %s from bridge %s: %s", errStateUnknown, rec.HostSide, host.MemberOf, err)
			}
		}
		// An epair(4) end is an ordinary Ethernet interface, so the
		// Bridge SVI case EnsureMember can now report (ADR-0138) is
		// unreachable for it. It is still treated as a failure rather
		// than waved through: MembershipSVI means "no addm was issued",
		// and continuing past that would record this jail's host side as
		// joined to a bridge it is provably not on - the same
		// silently-misplaced-network outcome the deletem above exists
		// to prevent, arriving by a different route.
		membership, err := r.Bridge.EnsureMember(ctx, addr.Bridge, rec.HostSide)
		if err != nil {
			return out, fmt.Errorf("%w: joining %s to bridge %s: %s", errStateUnknown, rec.HostSide, addr.Bridge, err)
		}
		if membership.State == vlan.MembershipUnknown || membership.State == vlan.MembershipSVI {
			return out, fmt.Errorf("%w: joining %s to bridge %s: membership state %q, which is not a confirmed join", errStateUnknown, rec.HostSide, addr.Bridge, membership.State)
		}
		if !contains(out.findings, FindingBridgeMembershipWrong) {
			out.findings = append(out.findings, FindingBridgeMembershipWrong)
		}
		out.repairs = append(out.repairs, fmt.Sprintf("moved host interface %s from bridge %q to %s", rec.HostSide, host.MemberOf, addr.Bridge))
	}

	if rec.Bridge != addr.Bridge {
		// The interface may be fine while the record still names the
		// old bridge; fixing that keeps the next pass's comparison
		// meaningful instead of re-reporting a move that already
		// happened.
		rec.Bridge = addr.Bridge
		state.Epairs[jailID] = rec
		if err := state.Save(); err != nil {
			return out, fmt.Errorf("%w: recording the corrected bridge for jail %q: %s", errStateUnknown, jailID, err)
		}
		out.repairs = append(out.repairs, fmt.Sprintf("recorded bridge %s for jail %q (was %q or unrecorded)", addr.Bridge, jailID, rec.Bridge))
	}

	return out, nil
}

// provision creates a fresh epair pair on the intended bridge and
// records it, reporting why through why (empty on a cold first
// provision). It is the single place a pair is ever created, so the
// record-then-adopt-orphan ordering that keeps a failed write from
// leaking an interface is stated once rather than twice.
func (r *Reconciler) provision(ctx context.Context, jailID string, addr Addressing, state State, out hostOutcome, why ...string) (hostOutcome, error) {
	if err := r.ensureBridge(ctx, addr); err != nil {
		return out, fmt.Errorf("%w: %s", errStateUnknown, err)
	}
	hostSide, jailSide, err := r.Bridge.EnsureEpair(ctx, addr.Bridge)
	if err != nil {
		return out, fmt.Errorf("%w: creating an epair on bridge %s: %s", errStateUnknown, addr.Bridge, err)
	}
	created := fmt.Sprintf("created epair %s/%s and joined %s to bridge %s", hostSide, jailSide, hostSide, addr.Bridge)
	if len(why) > 0 {
		created = why[0] + "; " + created
	}

	state.Epairs[jailID] = EpairRecord{HostSide: hostSide, JailSide: jailSide, Bridge: addr.Bridge}
	if err := state.Save(); err != nil {
		// The pair exists but nothing points at it, so the next tick
		// would create another. Destroying it here is what stops that;
		// if the destroy also fails the leak is real, and the verdict
		// says so rather than letting the node look clean.
		if derr := r.Bridge.DestroyEpair(ctx, hostSide); derr != nil {
			return out, fmt.Errorf("%w: recording the new epair for jail %q failed (%s) and destroying the unrecorded pair %s failed too (%s), so this node now has an orphaned interface no record points at", errStateUnknown, jailID, err, hostSide, derr)
		}
		return out, fmt.Errorf("%w: recording the new epair for jail %q failed and the unrecorded pair %s was destroyed, so the next tick will create another: %s", errStateUnknown, jailID, hostSide, err)
	}

	out.hostSide, out.jailSide, out.bridge = hostSide, jailSide, addr.Bridge
	out.repairs = append(out.repairs, created)
	return out, nil
}

// reconcileJail ensures the jail's own address and default route match
// the intent. A jail that is not running has nothing inside it to
// address - its addressing is applied at creation time by
// jail.Manager.CreateJail - so this is a no-op for one, reported as
// VerdictNotRunning rather than as a failure.
func (r *Reconciler) reconcileJail(ctx context.Context, jailID string, addr Addressing, host hostOutcome, res Result) Result {
	notRunning := func(detail string) Result {
		res.Verdict = VerdictNotRunning
		res.Observed = true
		res.Detail = detail
		return res
	}
	ready := fmt.Sprintf("jail %q is not running, so there is no in-jail networking to observe; its epair %s/%s is ready for it", jailID, host.hostSide, host.jailSide)

	running, err := r.Jail.JailExists(ctx, jailID)
	if err != nil {
		return unknown(res, fmt.Sprintf("%v: checking whether jail %q is running: %s", errStateUnknown, jailID, err))
	}
	if !running {
		return notRunning(ready)
	}

	// A jail with no assigned address manages its own. Observing and
	// then "repairing" it would mean deleting whatever it configured
	// for itself - which is the entire point of ADR-0117's
	// uplink_bridged carve-out, and is exactly the same carve-out
	// internal/raft's allocator already makes for VMs.
	if !addr.HasAddress() {
		res.Observed = true
		res.Verdict = settled(res)
		res.Detail = fmt.Sprintf("jail %q has no Apiary-assigned address (its network skips allocation), so its in-jail addressing is its own business and was deliberately left alone", jailID)
		return res
	}

	observed, err := r.Jail.ObserveNet(ctx, jailID, host.jailSide)
	if err != nil {
		if errors.Is(err, jail.ErrJailNotFound) {
			// It was running a moment ago and is not now. A definite,
			// different answer from unknown: the next tick creates the
			// jail with a good pair already waiting for it.
			return notRunning(fmt.Sprintf("jail %q stopped while its networking was being observed, so there was nothing to reconcile inside it; its epair %s/%s is ready for it", jailID, host.hostSide, host.jailSide))
		}
		return unknown(res, fmt.Sprintf("%v: observing %s inside jail %q: %s", errStateUnknown, host.jailSide, jailID, err))
	}
	if !observed.InterfacePresent {
		res.Verdict = VerdictDrifted
		res.Findings = append(res.Findings, FindingRestartRequired)
		res.Detail = fmt.Sprintf("jail %q is running but interface %s is not present inside it; jail(8) cannot add a vnet interface to an already-running jail, so this jail must be restarted", jailID, host.jailSide)
		return res
	}

	// Classify the addressing before repairing it, so the findings
	// describe what was actually observed rather than whatever the
	// repair happened to do. The repairs themselves are only *planned*
	// here and moved into Result.Repairs once jail.Manager has actually
	// made them: a failed repair that reported itself in Repairs would
	// be a lie in the one field a caller is most likely to trust
	// without re-reading the verdict.
	want := jail.Address{IP: addr.IP, PrefixLen: addr.PrefixLen}
	planned := make([]string, 0, 3)
	have := false
	for _, existing := range observed.Addresses {
		if existing == want {
			have = true
			continue
		}
		res.Findings = append(res.Findings, FindingForeignAddress)
		planned = append(planned, fmt.Sprintf("removed unassigned address %s from %s inside jail %q", existing, host.jailSide, jailID))
	}
	if !have {
		res.Findings = append(res.Findings, FindingAddressWrong)
		planned = append(planned, fmt.Sprintf("assigned %s/%d to %s inside jail %q", addr.IP, addr.PrefixLen, host.jailSide, jailID))
	}
	if addr.Gateway != "" && observed.DefaultRoute != addr.Gateway {
		res.Findings = append(res.Findings, FindingRouteWrong)
		planned = append(planned, fmt.Sprintf("set jail %q's default route to %s (was %s)", jailID, addr.Gateway, orNone(observed.DefaultRoute)))
	}

	if len(res.Findings) == 0 {
		res.Observed = true
		res.Verdict = settled(res)
		res.Detail = fmt.Sprintf("jail %q: epair %s/%s is up, joined to bridge %s, and inside the jail %s carries exactly %s/%d with %s", jailID, host.hostSide, host.jailSide, res.Bridge, host.jailSide, addr.IP, addr.PrefixLen, orNone(observed.DefaultRoute))
		return res
	}

	if err := r.Jail.EnsureAddressing(ctx, jailID, host.jailSide, want, addr.Gateway); err != nil {
		res.Verdict = VerdictDrifted
		res.Observed = true
		res.Detail = fmt.Sprintf("jail %q's networking needed repair (%s) and the repair failed: %s", jailID, strings.Join(findingStrings(res.Findings), ", "), err)
		return res
	}
	res.Repairs = append(res.Repairs, planned...)
	res.Verdict = VerdictRepaired
	res.Observed = true
	res.Detail = fmt.Sprintf("jail %q: repaired %s", jailID, strings.Join(findingStrings(res.Findings), ", "))
	return res
}

// settled picks between the two success verdicts once a pass has
// established that nothing is wrong. It keys off whether this pass
// actually changed anything rather than off whether drift was found,
// because the two are not the same question: a pass can find no
// in-jail drift while still having had to rebuild a dead epair pair, and
// reporting that as plain in_sync would hide a jail that needed
// rebuilding after every reboot.
func settled(res Result) Verdict {
	if len(res.Repairs) > 0 {
		return VerdictRepaired
	}
	return VerdictInSync
}

// ensureBridge makes sure the bridge this jail hangs off exists before
// anything tries to join it. A bridge destroyed out from under a running
// jail is one of the three drift cases this package exists for, and
// vlan.EnsureBridge is itself idempotent, so this is safe on every tick.
func (r *Reconciler) ensureBridge(ctx context.Context, addr Addressing) error {
	if addr.Bridge == "" {
		return fmt.Errorf("no bridge name: this jail names no network, so there is no bridge to attach its vnet interface to")
	}
	_, err := r.Bridge.EnsureBridge(ctx, addr.Bridge)
	return err
}

func unknown(res Result, detail string) Result {
	res.Observed = false
	res.Verdict = VerdictUnknown
	res.Detail = detail
	return res
}

func findingStrings(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, string(f))
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "no default route"
	}
	return s
}

func contains(list []Finding, want Finding) bool {
	for _, f := range list {
		if f == want {
			return true
		}
	}
	return false
}
