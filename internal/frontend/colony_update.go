package frontend

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"strings"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/colonyupdate"
	"github.com/glenjbarber/apiary/internal/health"
)

// The controlled, one-at-a-time Colony update page (ADR-0145).
//
// The architecture, stated once here because every decision below
// follows from it: **UI state is advisory; the backend independently
// enforces colony-wide single-flight.** The greyed-out controls on this
// page are a courtesy to the operator, never the mechanism. A second
// browser tab, a curl, or a stale page is refused by whatever implements
// colonyupdate.Controller, and this file's job is to render that refusal
// honestly when it arrives rather than to prevent it.
//
// Four consequences are load-bearing and easy to get wrong, so each is
// enforced in code and covered by a test:
//
//  1. The POST handler does NOT check the page's own idea of which Comb
//     is the target. It forwards the operator's intent to the backend
//     and renders whatever the backend then says. Gating on the rendered
//     nomination would make the UI the enforcement mechanism, which is
//     precisely the mistake ADR-0145 rejects.
//  2. Every render - page load, poll, and post-action alike - re-reads
//     the update state. Nothing is carried from a previous render, so a
//     reload, a second tab, or a stale page can only ever show what the
//     server says now.
//  3. A node ID displayed as the update target always comes from
//     colonyupdate.State, never from a submitted form. A forged or stale
//     client cannot make this page name a target the server did not name.
//  4. An update request is an Admin action, matching POST
//     /machine/services/{name}/restart, because an update restarts
//     managerd and raftd. See routes() for the registration.
//
// The only client-side refusals on the POST are the two that are about
// this frontend rather than about the Colony: the session's role, and
// whether the node ID is a current raft member (knownColonyMember, whose
// existing doc comment explains why that must be checked before the
// frontend's own credentials are allowed anywhere near a caller-supplied
// name).

// colonyUpdateTimeout bounds this page's own read of, or request to, the
// update system.
//
// It is separate from colonyLeaderStatusTimeout because the two answer
// different questions: the leader indicator runs on every page in the app
// and must stay cheap, while this one runs only on this page and its own
// poll. Exceeding it yields the unobserved verdict rather than holding a
// browser open - which is the correct outcome, since a reading that
// arrived too late to be current is not evidence of anything.
const colonyUpdateTimeout = 5 * time.Second

// ---------------------------------------------------------------------------
// Rendering an Outcome
// ---------------------------------------------------------------------------

// colonyUpdateOutcomeClass maps an outcome to a badge class from
// layout.html's vocabulary; colonyUpdateOutcomeLabel names it in words.
//
// Both live here rather than in the template for the reason
// statedigest.go and colony_leader.go already give: a template should
// render a verdict it was handed and never reason about evidence, and two
// places choosing colours independently is how one of them comes to
// disagree about which one is green.
//
// The mapping is a bijection onto six distinct classes, so the two
// inconclusive outcomes are distinguishable from each other and from
// every settled one, by colour as well as by name:
//
//   - confirmed-complete -> ready    green; the ONLY success
//   - in-progress       -> degraded  amber; still moving, not yet true
//   - failed            -> error    red; positive evidence of breakage
//   - blocked           -> stopped  grey; refused on purpose, nothing
//     attempted. Deliberately NOT the same colour as failed, because
//     nothing went wrong - the system said no, and that is the
//     recoverable outcome ADR-0145 calls for.
//   - unknown           -> unknown  a reading that does not settle it
//   - unobserved        -> stale    dashed; nothing was learned at all
//
// An outcome this build has never heard of renders as unobserved, the one
// bucket guaranteed to be honest about a value it cannot interpret,
// matching ADR-0056's rule that an unrecognised observation is never read
// as a healthy one.
func colonyUpdateOutcomeClass(o colonyupdate.Outcome) string {
	switch o {
	case colonyupdate.OutcomeConfirmedComplete:
		return "ready"
	case colonyupdate.OutcomeInProgress:
		return "degraded"
	case colonyupdate.OutcomeFailed:
		return "error"
	case colonyupdate.OutcomeBlocked:
		return "stopped"
	case colonyupdate.OutcomeUnknown:
		return "unknown"
	default:
		// OutcomeUnobserved, and anything unrecognised.
		return "stale"
	}
}

// colonyUpdateOutcomeLabel is the short text form. Every value has its own
// words, so no two outcomes can share a rendering by accident and the two
// inconclusive ones are always nameable out loud.
func colonyUpdateOutcomeLabel(o colonyupdate.Outcome) string {
	switch o {
	case colonyupdate.OutcomeConfirmedComplete:
		return "Confirmed complete"
	case colonyupdate.OutcomeInProgress:
		return "In progress"
	case colonyupdate.OutcomeFailed:
		return "Failed"
	case colonyupdate.OutcomeBlocked:
		return "Blocked"
	case colonyupdate.OutcomeUnknown:
		return "Unknown"
	case colonyupdate.OutcomeUnobserved:
		return "Unobserved"
	default:
		return "Unobserved"
	}
}

// colonyUpdateStepView is one step of the operation as the template
// receives it. Every field the template reads has already been decided
// here - the template makes no inferences.
type colonyUpdateStepView struct {
	Name string

	// Outcome is carried through for the data attribute, so a rendered
	// page can be checked for the exact vocabulary rather than for a
	// colour that might have come from anywhere.
	Outcome      string
	OutcomeLabel string
	OutcomeClass string
	Detail       string
	Evidence     string

	ObservedAt    string
	HasObservedAt bool
}

func colonyUpdateStepViewFrom(step colonyupdate.Step) colonyUpdateStepView {
	view := colonyUpdateStepView{
		Name:         step.Name,
		Outcome:      string(step.Outcome),
		OutcomeLabel: colonyUpdateOutcomeLabel(step.Outcome),
		OutcomeClass: colonyUpdateOutcomeClass(step.Outcome),
		Detail:       step.Detail,
		Evidence:     step.Evidence,
	}
	// A zero time means this step was not observed. Rendering it as
	// "1970-01-01" would be a fabricated reading, and presenting it as
	// "now" would be worse, so the timestamp is simply omitted and the
	// step's own outcome already carries the honest word.
	if !step.ObservedAt.IsZero() {
		view.ObservedAt = step.ObservedAt.Format("2006-01-02 15:04:05 MST")
		view.HasObservedAt = true
	}
	if view.Detail == "" && view.Evidence != "" {
		view.Detail = "no reason was recorded for this outcome; the evidence below is what the system reported"
	}
	return view
}

// colonyUpdateStateView is the colony-level answer as the template
// receives it.
type colonyUpdateStateView struct {
	// Observed is false when the update system could not be read at all.
	// Every other field here is then meaningless, and the template says
	// so rather than rendering a confident panel built out of nothing.
	Observed bool

	Phase      string
	PhaseLabel string
	PhaseClass string
	Detail     string

	OperationID string
	RequestedBy string

	StartedAt    string
	HasStartedAt bool

	// NominatedNodeID is the ONE Comb whose update control may be
	// enabled. Empty is a real, explained state, never a missing value.
	NominatedNodeID string
	NominatedDetail string

	SingleFlightHolder string
	BackendEnforced    bool
	EnforcedDetail     string

	// EnforcedHeadline is the sentence the page leads with, chosen from
	// what the update system itself reports rather than from what the
	// page happened to disable.
	EnforcedHeadline string

	// Contradiction names a self-inconsistent reading rather than
	// resolving it. Empty on any coherent state.
	Contradiction string

	UnavailableReason string
	Steps             []colonyUpdateStepView

	// InFlight is true only when the system says an operation is running
	// AND names a target. It is what leaves no control enabled at all
	// while an update runs.
	InFlight bool

	// AnyEnabled reports whether exactly one control is enabled
	// somewhere on the page - true only in the one case where the update
	// system has derived a target to offer and nothing is in flight.
	AnyEnabled bool
}

// colonyUpdateEnforcedHeadline is the sentence the page leads with.
//
// It is chosen from BackendEnforced, not from what the page disabled. A
// page whose controls are greyed out because nothing is attached must
// not describe itself as protected, and a page that hard-codes "the
// server enforces this" would be claiming an enforcement that does not
// exist yet.
func colonyUpdateEnforcedHeadline(observed, enforced bool) string {
	switch {
	case !observed:
		return "No Colony update system is attached, so nothing here is enforced and nothing here can start an update."
	case enforced:
		return "Colony-wide single-flight is enforced by the server, independently of this page. Greying out the " +
			"other Combs is a courtesy to you, not the mechanism: a second tab, a stale page, or a direct request " +
			"is refused by the backend."
	default:
		return "This update system does not report that it enforces colony-wide single-flight. Treat the greyed-out " +
			"controls as cosmetic and assume nothing is stopping a concurrent request."
	}
}

// colonyUpdateStateViewFrom converts a colonyupdate.State into the view
// the template renders.
//
// It is pure and total: every field of State is accounted for and the
// conversion cannot fail. That matters, because the one thing this page
// must never do is invent state - a conversion that had to guess would be
// a guess rendered as a fact.
func colonyUpdateStateViewFrom(state colonyupdate.State) colonyUpdateStateView {
	view := colonyUpdateStateView{
		Observed:           state.Observed,
		Phase:              string(state.Phase),
		PhaseLabel:         colonyUpdateOutcomeLabel(state.Phase),
		PhaseClass:         colonyUpdateOutcomeClass(state.Phase),
		Detail:             state.Detail,
		OperationID:        state.OperationID,
		RequestedBy:        state.RequestedBy,
		NominatedNodeID:    state.NominatedNodeID,
		NominatedDetail:    state.NominatedDetail,
		SingleFlightHolder: state.SingleFlightHolder,
		BackendEnforced:    state.BackendEnforced,
		EnforcedDetail:     state.EnforcedDetail,
		EnforcedHeadline:   colonyUpdateEnforcedHeadline(state.Observed, state.BackendEnforced),
		UnavailableReason:  state.UnavailableReason,
	}

	if !state.StartedAt.IsZero() {
		view.StartedAt = state.StartedAt.Format("2006-01-02 15:04:05 MST")
		view.HasStartedAt = true
	}

	for _, step := range state.Steps {
		view.Steps = append(view.Steps, colonyUpdateStepViewFrom(step))
	}

	// An operation is in flight only when the system says so AND names a
	// target. A phase claiming in-progress with no target is a
	// contradiction; resolving it either way would be a guess - enable
	// everything because there is nothing to be in flight on, or
	// disable everything because something is - and neither would be
	// honest. So it is reported as the contradiction it is and no control
	// is enabled.
	inFlightPhase := state.Observed && state.Phase == colonyupdate.OutcomeInProgress
	switch {
	case inFlightPhase && state.TargetNodeID == "":
		view.Contradiction = "the update system reports an operation in progress but names no Comb, so this page " +
			"cannot say which one is being updated and no control is enabled"
	case inFlightPhase:
		view.InFlight = true
	}

	// Exactly one control is enabled, and only in the one case where the
	// update system has derived a target to offer. Note what is NOT a
	// reason: this is not "the operator may pick", and it is not "the
	// page decided the order". The nomination is the system's own,
	// re-derived on this read - ADR-0145's "leadership is a derived
	// fact, re-derived at execution time". With no nomination, nothing
	// is enabled and the page says so.
	view.AnyEnabled = state.Observed && !view.InFlight && state.NominatedNodeID != ""

	return view
}

// colonyUpdateUnreadableState is the honest State for a read that failed.
//
// It is a named constructor rather than an inline literal at each call
// site so that the two things it must never do - invent a nomination, and
// imply enforcement - are stated once and cannot drift apart.
func colonyUpdateUnreadableState(reason string) colonyupdate.State {
	return colonyupdate.State{
		Observed:           false,
		UnavailableReason:  reason,
		Phase:              colonyupdate.OutcomeUnobserved,
		Detail:             "the Colony update system could not be read: " + reason + ". Nothing below describes a real update state",
		NominatedNodeID:    "",
		NominatedDetail:    "nothing was read, so no Comb can be offered",
		BackendEnforced:    false,
		EnforcedDetail:     "nothing was read, so whether the update path enforces colony-wide single-flight is unobserved",
		SingleFlightHolder: "",
		StartedAt:          time.Time{},
		Steps:              nil,
	}
}

// ---------------------------------------------------------------------------
// Per-Comb rows
// ---------------------------------------------------------------------------

// colonyUpdateCombView is one row: a Comb, what is known about it right
// now, and whether its update control may be clicked.
type colonyUpdateCombView struct {
	NodeID string

	// Reachable, HealthStatus, HealthExplanation and the digest fields
	// come straight from the same clusterNodeView the command center
	// builds (see clusterNodeEvidence and stateDigestVerdicts), so this
	// page cannot disagree with "/" about whether a Comb is healthy or
	// whether the FSMs agree. Re-deriving health here would mean two
	// answers to the same question on two pages.
	Reachable         bool
	HealthStatus      string
	HealthExplanation string

	IsColonyLeader   bool
	DigestBadgeClass string
	DigestBadgeLabel string
	DigestDetail     string

	// RoleLabel names the role this Comb is known to be playing. It is
	// read from evidence already gathered and is deliberately NOT a
	// control: ADR-0145 is explicit that the operator is never shown a
	// leader as a choice, so the role is information and never feeds the
	// decision about which control is enabled.
	RoleLabel string

	// UpdateLabel/UpdateClass/UpdateDetail are this Comb's own current
	// update state, which is not always the colony phase: a Comb that
	// was confirmed complete stays confirmed while the next Comb is in
	// progress.
	UpdateLabel  string
	UpdateClass  string
	UpdateDetail string

	// IsTarget marks the Comb the colony is updating right now, taken
	// from colonyupdate.State and never from a submitted form.
	IsTarget bool

	// CanUpdate is the single enabled control on the whole page, when
	// there is one: an Admin session, an observed system with a
	// nomination, no operation in flight, and this Comb being the
	// nominated one.
	CanUpdate bool

	// DisabledReason is always set when CanUpdate is false and always
	// says which condition failed. A greyed-out control with no
	// explanation is an operator being told nothing.
	DisabledReason string
}

// colonyUpdateCombRows builds the page's rows: every Comb in the colony,
// each carrying the evidence the command center already computed, plus
// the update verdict that belongs to that Comb specifically.
//
// combs is the already-built clusterNodeView slice (complete, membership-
// verified, one per Comb) and state is a fresh colonyupdate.State. The
// function is pure, so every combination - including the awkward ones -
// is reachable by test without a live cluster.
func colonyUpdateCombRows(combs []clusterNodeView, state colonyupdate.State, canOperate bool) []colonyUpdateCombView {
	colony := colonyUpdateStateViewFrom(state)

	rows := make([]colonyUpdateCombView, 0, len(combs))
	for _, comb := range combs {
		row := colonyUpdateCombView{
			NodeID:            comb.NodeID,
			Reachable:         comb.Reachable,
			HealthStatus:      string(comb.HealthStatus),
			HealthExplanation: comb.HealthExplanation,
			IsColonyLeader:    comb.IsColonyLeader,
			DigestBadgeClass:  comb.DigestBadgeClass,
			DigestBadgeLabel:  comb.DigestBadgeLabel,
			DigestDetail:      comb.DigestDetail,
			RoleLabel:         combRoleLabel(comb),
		}
		row.IsTarget = state.Observed && state.TargetNodeID != "" && state.TargetNodeID == comb.NodeID
		row.UpdateLabel, row.UpdateClass, row.UpdateDetail = combUpdateVerdict(comb.NodeID, state, row.IsTarget)
		row.CanUpdate, row.DisabledReason = combControlAvailability(comb, state, colony, canOperate)
		rows = append(rows, row)
	}
	return rows
}

// combRoleLabel names what is known about one Comb's role in the Colony.
//
// A Comb's role is only ever read, never chosen: it comes from the
// leader reading the colony overview already made, from this Comb's own
// reachability, and from its computed health verdict. When none of those
// establish a role, the row says so in words rather than showing an
// empty cell, which would read as "no role" - a claim this page cannot
// make.
func combRoleLabel(comb clusterNodeView) string {
	if comb.IsColonyLeader {
		return "Leader"
	}
	if comb.Reachable {
		return "Follower"
	}
	if comb.HealthStatus == health.StatusUnknown || comb.HealthStatus == health.StatusStale {
		return "Role unobserved"
	}
	return "Not observed as a voter"
}

// combUpdateVerdict is this Comb's own current update state.
//
// The colony phase answers "what is the operation doing"; this answers
// "what is true of this Comb", and the two genuinely differ. A Comb
// confirmed complete a moment ago is still confirmed complete while the
// next Comb is being restarted, and painting the colony phase onto every
// row would overwrite that history with the present tense. When the
// system offers no per-Comb information at all - the case for the inert
// implementation, and for any system holding only a colony-level view -
// every row says so identically rather than implying a per-Comb reading
// nobody made.
func combUpdateVerdict(nodeID string, state colonyupdate.State, isTarget bool) (label, class, detail string) {
	if !state.Observed {
		return "Unobserved", "stale", "no Colony update system is attached, so this Comb's update state cannot be read"
	}
	if isTarget {
		return colonyUpdateOutcomeLabel(state.Phase), colonyUpdateOutcomeClass(state.Phase), state.Detail
	}
	return "No update recorded", "stale", "the update system reports no update for this Comb: it is not the Comb " +
		"currently being updated, and it is not the Comb on offer"
}

// combControlAvailability decides whether ONE row's Update control is
// enabled, and always says why not.
//
// Every clause is a statement about the server's current reading, never
// about a previous render and never about what this browser clicked. The
// pair returned is (false, reason) for every disabled row: there is no
// path that disables a control without explaining it.
//
// Note what is deliberately absent: this function does not consult the
// colony's health verdict, the digest, or the role of any Comb. ADR-0145
// puts the health gate inside the update system, evaluated per step at
// execution time, and a UI that second-guessed it would be a second,
// disagreeing gate - and a stale one.
func combControlAvailability(comb clusterNodeView, state colonyupdate.State, colony colonyUpdateStateView, canOperate bool) (enabled bool, reason string) {
	if !canOperate {
		return false, "your role does not permit starting a Colony update; this page is read-only for you"
	}
	if !state.Observed {
		return false, "no Colony update system is attached, so this control would do nothing"
	}
	if colony.Contradiction != "" {
		return false, "the update system's own reading is inconsistent, so this page enables nothing rather than guess"
	}
	if colony.InFlight {
		return false, "another Comb is being updated right now, so the rest of the Colony waits. This is a courtesy " +
			"from this page, not the rule that stops it: the server refuses a concurrent request on its own"
	}
	if state.NominatedNodeID == "" {
		if state.NominatedDetail != "" {
			return false, "no Comb is on offer right now: " + state.NominatedDetail
		}
		return false, "the update system is offering no Comb to update right now"
	}
	if state.NominatedNodeID != comb.NodeID {
		return false, "the update system is offering " + state.NominatedNodeID + " right now. Which Comb goes first is " +
			"derived by the system at the moment of use, not chosen here, and it can change before anything happens"
	}
	if !comb.Reachable {
		// Reachable is a real, current observation, and naming it is
		// more use to an operator than a generic refusal. It is a
		// courtesy, not an enforcement: the backend may hold a better
		// reading, and this page is not in a position to overrule it.
		return false, "this Comb's managerd did not answer a moment ago, so this page has no evidence it is reachable"
	}
	return true, ""
}

// ---------------------------------------------------------------------------
// Reading the update system
// ---------------------------------------------------------------------------

// colonyUpdateController returns the configured Controller, or the inert
// one when none was wired.
//
// The fallback is the point: a frontend built before the real adapter
// exists serves a page that says, in words, that it is inert. It does
// not serve a page claiming there is nothing to update and offering no
// explanation, and it certainly does not serve a page that pretends an
// update system is attached.
func (s *Server) colonyUpdateController() colonyupdate.Controller {
	if s.colonyUpdate != nil {
		return s.colonyUpdate
	}
	return colonyupdate.Inert{}
}

// readColonyUpdateState performs this page's read of the update system
// under its own timeout, and converts a failure into the honest
// unobserved verdict rather than into a zero State.
//
// This exists as a function rather than inline because the distinction it
// preserves is the whole page: a nil error with a zero State would render
// as a working system with nothing to report, which is a lie, while a
// failed read renders as a system that could not be asked.
func (s *Server) readColonyUpdateState(ctx context.Context) colonyupdate.State {
	ctx, cancel := context.WithTimeout(ctx, colonyUpdateTimeout)
	defer cancel()

	state, err := s.colonyUpdateController().ColonyUpdateState(ctx)
	if err != nil {
		reason := err.Error()
		if errors.Is(err, colonyupdate.ErrNoImplementation) {
			reason = "no Colony update system is attached to this frontend"
		}
		return colonyUpdateUnreadableState(reason)
	}
	return state
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// colonyUpdateRender bundles one render's worth of update-page data with
// the StatusResponse it already fetched, so the full-page render can hand
// that response to withAuthFieldsFrom and avoid paying for a second
// Status RPC - the same economy handleClusterOverviewPage already follows.
type colonyUpdateRender struct {
	page      pageData
	status    *rpcpb.StatusResponse
	statusErr error
}

// handleColonyUpdatePage serves GET /colony-update.
//
// It is a separate page rather than another panel on the command center,
// for three reasons, all about honesty rather than layout:
//
//   - The command center's Comb cards are a live topology summary, and
//     cluster_overview.html packs a whole card onto one line inside the
//     cockpit-topology-list wrapper. An operation lasting minutes, with
//     per-step outcomes and a single changing enabled control, does not
//     belong in a summary an operator reads at a glance.
//   - This page polls; the command center does not. Adding a poll to the
//     most-visited page in the app, which already gathers every Comb's
//     live evidence on each load, would change its cost for everyone.
//   - A failed update and a healthy Colony look nothing alike, and
//     burying a failure inside a summary makes it the thing an operator
//     skims past.
func (s *Server) handleColonyUpdatePage(w http.ResponseWriter, r *http.Request) {
	render := s.colonyUpdateView(r)
	s.render(w, "colony_update_page", s.withAuthFieldsFrom(r, render.page, render.status, render.statusErr))
}

// handleColonyUpdatePanel serves GET /colony-update/panel, the htmx poll
// target for the live panel on that page.
//
// It exists so the progress view advances on its own, using the vendored
// htmx idiom this project already uses (vms.html polls its tbody every
// three seconds) rather than any new client-side code. Because it goes
// through the same ServeHTTP session gate as every other route, an
// expired session polling it receives an HX-Redirect to the login page
// rather than a bare 302 - the HTMX-aware pattern redirectToLogin
// already implements.
//
// It renders the same view the full page renders, from its own fresh
// read, so a poll and a reload can never disagree.
func (s *Server) handleColonyUpdatePanel(w http.ResponseWriter, r *http.Request) {
	render := s.colonyUpdateView(r)
	s.render(w, "colony_update_panel", s.withAuthFieldsForFragment(r, render.page))
}

// colonyUpdateView gathers everything one render of this page needs.
//
// The order is deliberate. Membership first, because a page that claims to
// list every Comb while listing none is the one thing it must never do.
// Per-Comb evidence second, reusing the command center's own code so the
// two pages cannot disagree. The update system last, so the nomination is
// checked against membership after it arrives.
func (s *Server) colonyUpdateView(r *http.Request) colonyUpdateRender {
	status, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if statusErr != nil {
		return colonyUpdateRender{
			page: pageData{
				Error:      "could not read current Colony membership: " + statusErr.Error(),
				ActivePage: "colony-update",
				ColonyUpdate: colonyUpdateStateViewFrom(colonyUpdateUnreadableState(
					"Colony membership could not be read, so no Comb can be listed")),
			},
			statusErr: statusErr,
		}
	}

	localNodeID := status.GetManagerNodeId()
	nodeIDs := status.GetKnownNodeIds()
	if len(nodeIDs) == 0 && localNodeID != "" {
		nodeIDs = []string{localNodeID}
	}

	now := time.Now()
	index := s.gatherCombCauseIndex(r.Context())
	combs := make([]clusterNodeView, len(nodeIDs))
	var wg sync.WaitGroup
	for i, id := range nodeIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			combs[i] = s.clusterNodeEvidence(r.Context(), id, localNodeID, status, index, now)
		}(i, id)
	}
	wg.Wait()
	sort.Slice(combs, func(i, j int) bool { return combs[i].NodeID < combs[j].NodeID })

	observations := make([]stateDigestObservation, len(combs))
	for i, comb := range combs {
		observations[i] = stateDigestObservation{NodeID: comb.NodeID, Digest: comb.StateDigest, AppliedIndex: comb.AppliedIndex}
	}
	digestViews, digestColony := stateDigestVerdicts(observations)
	for i := range combs {
		view, found := digestViews[combs[i].NodeID]
		if !found {
			// Unreachable while both are built from the same rows, but an
			// absent verdict must render as unobserved rather than as a
			// blank badge if that ever stops holding.
			view = stateDigestUnobservedView(stateDigestObservation{NodeID: combs[i].NodeID}, len(combs))
		}
		combs[i].DigestState = view.State
		combs[i].DigestBadgeClass = view.BadgeClass
		combs[i].DigestBadgeLabel = view.Label
		combs[i].DigestDetail = view.Detail
	}

	// One fresh read of the update system, after membership. A stale
	// nomination naming a Comb that has since left the Colony is dropped
	// rather than rendered, because a control pointing at a departed Comb
	// is worse than no control at all.
	state := s.readColonyUpdateState(r.Context())
	if state.NominatedNodeID != "" && !knownColonyMember(status, state.NominatedNodeID) {
		state.NominatedNodeID = ""
		state.NominatedDetail = "the Comb the update system offered is not a current member of this Colony, so no Comb " +
			"is on offer until the update system re-reads membership"
	}

	_, canOperate := s.currentSession(r)
	if s.auth == nil {
		canOperate = true
	}

	rows := colonyUpdateCombRows(combs, state, canOperate)
	enabled := 0
	for _, row := range rows {
		if row.CanUpdate {
			enabled++
		}
	}

	return colonyUpdateRender{
		page: pageData{
			ActivePage:                  "colony-update",
			ColonyUpdate:                colonyUpdateStateViewFrom(state),
			ColonyUpdateCombs:           rows,
			ColonyUpdateEnabledControls: enabled,
			StateDigestColony:           digestColony,
		},
		status: status,
	}
}

// handleColonyUpdateRequest serves POST /colony-update/request: the
// operator clicked Update on one Comb.
//
// The order of operations below is the design, not an implementation
// detail:
//
//  1. Current membership. A node ID arrives from a browser and is
//     therefore attacker controlled; knownColonyMember compares it
//     against raft's own current server configuration before anything
//     else happens, which is the same check handleHostPage already makes
//     for the same reason.
//  2. Forward the intent. The page's own notion of which Comb is
//     nominated is deliberately NOT consulted here. Checking it would
//     make the rendered page the enforcement mechanism, which is exactly
//     the mistake ADR-0145 rejects in the strongest terms.
//  3. Re-read, then render. Whatever the backend answered - a new state,
//     a refusal, or an error - the page is rendered from a fresh read
//     afterwards, so the target it names and the controls it enables are
//     the server's current answer and never the form's.
//
// There is no htmx here on purpose. A plain form POST with a full page
// reload is the honest shape for an action whose entire purpose is to
// make the browser re-read the truth, and it is what every other
// state-changing form in this app already does.
func (s *Server) handleColonyUpdateRequest(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimSpace(r.FormValue("node_id"))

	status, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if statusErr != nil {
		http.Error(w, "could not verify current Colony membership: "+statusErr.Error(), http.StatusServiceUnavailable)
		return
	}
	if !knownColonyMember(status, nodeID) {
		http.Error(w, "not a current member of this Colony: "+nodeID, http.StatusNotFound)
		return
	}

	actor := "unknown"
	if info, ok := s.currentSession(r); ok {
		actor = info.username
	}

	ctx, cancel := context.WithTimeout(r.Context(), colonyUpdateTimeout)
	defer cancel()
	_, err := s.colonyUpdateController().RequestColonyUpdate(ctx, colonyupdate.Request{
		NodeID:      nodeID,
		RequestedBy: actor,
	})

	// The refusal path. A *RefusedError is the backend declining, not a
	// failure of anything: nothing was attempted, and the Colony is as it
	// was. The backend's own words are shown verbatim rather than
	// paraphrased, and the page is then re-read so the controls on it
	// reflect the state the refusal left behind.
	var refused *colonyupdate.RefusedError
	switch {
	case errors.As(err, &refused):
		s.renderColonyUpdateAfterAction(w, r, status, colonyUpdateAfterAction{
			Refused:       true,
			RequestedNode: nodeID,
			Refusal:       refused.Error(),
			RefusalHold:   refused.Holder,
		})
	case errors.Is(err, colonyupdate.ErrNoImplementation):
		s.renderColonyUpdateAfterAction(w, r, status, colonyUpdateAfterAction{
			Refused:       true,
			RequestedNode: nodeID,
			Refusal: "no Colony update system is attached to this frontend, so the request could not be recorded. " +
				"Nothing was attempted against any Comb",
		})
	case err != nil:
		s.renderColonyUpdateAfterAction(w, r, status, colonyUpdateAfterAction{RequestedNode: nodeID, ActionError: err.Error()})
	default:
		s.renderColonyUpdateAfterAction(w, r, status, colonyUpdateAfterAction{RequestedNode: nodeID})
	}
}

// colonyUpdateAfterAction is what a POST is allowed to tell the page it
// then re-rendered.
//
// It carries only server-side facts, and in particular carries no success
// claim of its own: whether an update started is decided by the state the
// page re-reads, never by the fact that a POST returned without an error.
// RefusedNode is the Comb the operator actually clicked, so it is the
// operator's input, and it is rendered as what they asked for - never as
// a target.
type colonyUpdateAfterAction struct {
	Refused bool

	// RequestedNode is the Comb the operator's form named, recorded
	// whatever the outcome. It is echoed back as their request and is
	// never rendered as a target, which is why it is populated on the
	// accepted path too and not only on the refusal paths.
	RequestedNode string

	Refusal     string
	RefusalHold string

	// ActionError is a genuine failure to reach or act, distinct from a
	// refusal. It is shown as an error rather than as a verdict, because
	// "the request could not be delivered" is not "the request was
	// declined".
	ActionError string
}

// renderColonyUpdateAfterAction re-renders the full update page from a
// FRESH read after a POST, carrying the action's outcome.
//
// The fresh read is the whole point. Whatever the POST returned, the
// page's notion of the target, the phase, and which control is enabled
// all come from the state read here and now. A browser that submits a
// stale node ID, replays a double-click, or forges the form gets exactly
// the page everyone else gets: the truth.
func (s *Server) renderColonyUpdateAfterAction(w http.ResponseWriter, r *http.Request, status *rpcpb.StatusResponse, action colonyUpdateAfterAction) {
	render := s.colonyUpdateView(r)
	render.page.ColonyUpdateRequested = action.RequestedNode
	render.page.ColonyUpdateRefused = action.Refused
	render.page.ColonyUpdateRefusal = action.Refusal
	render.page.ColonyUpdateRefusalHolder = action.RefusalHold
	render.page.ColonyUpdateActionError = action.ActionError
	if render.status == nil {
		render.status = status
	}
	s.render(w, "colony_update_page", s.withAuthFieldsFrom(r, render.page, render.status, render.statusErr))
}

// ---------------------------------------------------------------------------
// Markup helpers used by the tests
// ---------------------------------------------------------------------------

// colonyUpdateControlAttribute marks every Update control on the page.
//
// It exists so a test can count the controls an operator could actually
// click without counting anything else the template emits, and so the
// marker is decided here rather than spelled independently in the
// template and the test - the two drifting apart is how a test ends up
// asserting on markup that no longer exists.
const colonyUpdateControlAttribute = `data-comb-update=`

// colonyUpdateControls returns each rendered Update control's opening
// tag, individually, so a caller can inspect its attributes.
func colonyUpdateControls(renderedHTML string) []string {
	var controls []string
	for offset := 0; ; {
		found := strings.Index(renderedHTML[offset:], colonyUpdateControlAttribute)
		if found < 0 {
			return controls
		}
		start := strings.LastIndexByte(renderedHTML[:offset+found], '<')
		if start < 0 {
			offset += found + len(colonyUpdateControlAttribute)
			continue
		}
		end := strings.IndexByte(renderedHTML[offset+found:], '>')
		if end < 0 {
			return controls
		}
		controls = append(controls, renderedHTML[start:offset+found+end+1])
		offset += found + end + 1
	}
}

// colonyUpdateEnabledControls counts the Update controls a rendered page
// offers WITHOUT the disabled attribute - which is the attribute the
// browser acts on, and therefore the only honest definition of
// "clickable". Counting the absence of a CSS class instead would prove
// nothing about what an operator could do.
func colonyUpdateEnabledControls(renderedHTML string) int {
	count := 0
	for _, control := range colonyUpdateControls(renderedHTML) {
		if !strings.Contains(control, " disabled") {
			count++
		}
	}
	return count
}
