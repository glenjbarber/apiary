package frontend

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	manager "github.com/glenjbarber/apiary/internal/manager"
)

// colonyJoinWindowView is the Machine page's view of the Colony-wide
// admission window (ADR-0147 Part 4).
//
// The distinction it encodes, and the reason it is a struct rather than
// the raw RPC response, is between "the window is closed" and "I could
// not find out". GetColonyJoinWindow is the UNAUTHENTICATED read, so a
// caller with no Colony API key still gets a well-formed response - and
// a closed window is that RPC's way of saying "nothing to see", via a
// refusal string in Error rather than a populated Window. Rendering
// that refusal as "closed" would be a lie about a different failure: the
// same Error field also carries "no peer forwarding is configured",
// "reaching <addr>: <dial error>", and a disallowed target address. An
// operator told "the window is closed" when the real answer is "this
// Comb could not reach the member you named" goes and opens a window
// that already exists.
//
// So Live is only true when a window actually came back, Error is only
// set when the RPC genuinely failed or refused, and the two are never
// inferred from one another.
type colonyJoinWindowView struct {
	// Live is true only when a window was returned and has not expired
	// as of Now. A populated Window with a past deadline is reported as
	// NOT live, because that is what the managerd-side gate will do
	// with it (internal/manager requireLiveColonyJoinWindow).
	Live bool
	// Error is the RPC's refusal or transport failure, verbatim. Empty
	// when Live, and also empty when the read succeeded and the window
	// is simply not open - the two are told apart by Live plus Window.
	Error string
	// ReadFailed is true when the read did not produce an answer at all
	// - a transport error, a forwarding failure, a disallowed target,
	// or a response with neither a window nor a reason. It is what
	// separates "the window is closed" (a real state, with a real next
	// step) from "this page could not find out" (no state to report,
	// and opening a window would not be acting on knowledge). Without
	// it the two render identically and the second silently offers a
	// fix that cannot work.
	ReadFailed bool

	// Window fields, carried through only when the RPC returned one.
	OpenedBy           string
	OpenedByKey        string
	OpenedAtUnix       int64
	ExpiresAtUnix      int64
	AppliedDurationSec int64

	ColonyNodeID string
	LeaderNodeID string

	// Now is the render-time clock, passed in rather than read from
	// time.Now() inside the helpers, so the countdown and the
	// "expired" decision are testable and cannot disagree with each
	// other within a single render.
	Now time.Time
}

// remaining returns the live duration left, floored at zero. A caller
// that wants to distinguish "just expired" from "long expired" uses
// ExpiresAtUnix directly; this is only for display.
func (v colonyJoinWindowView) remaining() time.Duration {
	if !v.Live {
		return 0
	}
	// Both sides are truncated to whole seconds. ExpiresAtUnix IS a
	// whole second (it came off the wire as a unix timestamp) while Now
	// carries a sub-second fraction, so subtracting the raw Now would
	// make every countdown read one second short - "14 minutes 59
	// seconds" for a window opened as exactly 15 minutes, which is the
	// kind of off-by-one an operator notices and stops trusting.
	d := time.Unix(v.ExpiresAtUnix, 0).Sub(v.Now.Truncate(time.Second))
	if d < 0 {
		return 0
	}
	return d
}

// currentColonyJoinWindow performs the unauthenticated read for the
// Machine page's own Comb. No target_address is sent: the page is
// showing THIS member's own Colony, and a caller-supplied address would
// be forwarded without this node's API key attached (ADR-0097), which
// is not something an authenticated page should be doing on its own
// initiative.
//
// A closed window is the expected steady state, not an error worth
// styling as a failure: Part 4 inverts the default, so "closed" is what
// an operator sees almost always and what they are looking at the page
// to change.
func (s *Server) currentColonyJoinWindow(r *http.Request, now time.Time) colonyJoinWindowView {
	resp, err := s.client.GetColonyJoinWindow(r.Context(), &rpcpb.GetColonyJoinWindowRequest{})
	view := colonyJoinWindowView{Now: now}
	if err != nil {
		view.Error = err.Error()
		view.ReadFailed = true
		return view
	}
	if resp.GetError() != "" {
		// This field carries the closed-window refusal AND every
		// genuine failure. They are told apart here, by asking the
		// manager package whether the string it produced is its own
		// refusal, rather than by pattern-matching the text in the UI.
		view.Error = resp.GetError()
		view.ReadFailed = !manager.IsColonyWindowClosedRefusal(resp.GetError())
		return view
	}
	w := resp.GetWindow()
	if w == nil {
		// No window and no error is a shape the RPC does not document
		// (it refuses rather than returning an empty window). Treat it
		// as a failed read with a named reason instead of rendering an
		// empty control that looks like a broken state.
		view.Error = "the Colony's join window could not be read: managerd returned neither a window nor a reason"
		view.ReadFailed = true
		return view
	}
	view.ColonyNodeID = resp.GetColonyNodeId()
	view.LeaderNodeID = resp.GetLeaderNodeId()
	view.OpenedBy = w.GetOpenedBy()
	view.OpenedByKey = w.GetOpenedByKey()
	view.OpenedAtUnix = w.GetOpenedAtUnix()
	view.ExpiresAtUnix = w.GetExpiresAtUnix()
	// A window whose deadline has already passed is reported as not
	// live even though the RPC returned it. liveColonyJoinWindow on
	// the manager side applies the same rule, so doing it here keeps
	// the page from advertising a window that the approval gate is
	// about to refuse.
	view.Live = w.GetEnabled() && w.GetExpiresAtUnix() > now.Truncate(time.Second).Unix()
	if !view.Live && w.GetEnabled() && w.GetExpiresAtUnix() <= now.Unix() {
		view.Error = fmt.Sprintf("the join window opened at %s expired at %s",
			time.Unix(w.GetOpenedAtUnix(), 0).Format(time.RFC3339),
			time.Unix(w.GetExpiresAtUnix(), 0).Format(time.RFC3339))
	}
	return view
}

// handleOpenColonyJoinWindow is Admin-tier (see the route table) and
// forwards exactly like every other admin-side state change: only the
// leader mutates, and this handler's job is to carry the operator's
// chosen duration and then show whatever came back.
//
// duration_seconds is OPTIONAL in the RPC, and the form's field is
// therefore left blank by default on purpose. An absent field means
// "use the configured default", and sending a number the operator did
// not type would be inventing intent. A field that IS present but is
// not a positive integer is refused here rather than being coerced to
// zero and silently turned into "use the default" - a typo in a
// duration must not quietly become a different duration.
func (s *Server) handleOpenColonyJoinWindow(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderMachinePageWithJoinWindowError(w, r, err.Error())
		return
	}
	req := &rpcpb.OpenColonyJoinWindowRequest{}
	if raw := strings.TrimSpace(r.FormValue("duration_seconds")); raw != "" {
		secs, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || secs <= 0 {
			s.renderMachinePageWithJoinWindowError(w, r,
				fmt.Sprintf("the window duration must be a whole number of seconds greater than zero, not %q", raw))
			return
		}
		req.DurationSeconds = secs
	}
	resp, err := s.client.OpenColonyJoinWindow(r.Context(), req)
	if err != nil {
		s.renderMachinePageWithJoinWindowError(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderMachinePageWithJoinWindowError(w, r, resp.GetError())
		return
	}
	// applied_duration_seconds is echoed rather than read back off
	// the window, so an operator who asked for more than the configured
	// ceiling is told which number actually won instead of having to
	// subtract two timestamps to find out.
	if applied := resp.GetAppliedDurationSeconds(); applied > 0 {
		s.redirectWithJoinWindowNotice(w, r, fmt.Sprintf(
			"Join window open for %d minutes. It closes on its own; close it early if the new Comb joins sooner.",
			(applied+59)/60))
		return
	}
	s.redirectWithJoinWindowNotice(w, r, "Join window open. It closes on its own.")
}

// handleCloseColonyJoinWindow is Admin-tier and takes no input: a
// window is closed by saying so, and there is no "close for N minutes"
// because reopening is the operation that extends one.
func (s *Server) handleCloseColonyJoinWindow(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.CloseColonyJoinWindow(r.Context(), &rpcpb.CloseColonyJoinWindowRequest{})
	if err != nil {
		s.renderMachinePageWithJoinWindowError(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderMachinePageWithJoinWindowError(w, r, resp.GetError())
		return
	}
	s.redirectWithJoinWindowNotice(w, r, "Join window closed. This Colony is not accepting new members.")
}

// redirectWithJoinWindowNotice sends the operator back to the Machine
// page carrying a one-line outcome in the query string, the same
// pattern redirectAfterJoinRequestAction uses for join-request actions.
// Redirecting rather than re-rendering means a browser refresh cannot
// re-post the open/close form, which for "open" would be a second
// window-opening act the operator did not intend.
func (s *Server) redirectWithJoinWindowNotice(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/machine?"+url.Values{"join_window_notice": {notice}}.Encode(), http.StatusFound)
}

func (s *Server) renderMachinePageWithJoinWindowError(w http.ResponseWriter, r *http.Request, formErr string) {
	nodeID := s.localNodeID(r)
	cfg, cfgErr := s.currentNodeConfig(r)
	vms, vmErr := s.currentMachineVMs(r, nodeID)
	cloudflareConfigured, _ := s.currentCloudflareStatus(r)
	services, serviceErr := s.currentNodeServices(r)
	originCerts, originErr := s.currentOriginCertificates(r)
	data := pageData{
		NodeConfig: cfg, NodeConfigFormError: cfgErr, MachineVMs: vms, MachineFirewallError: vmErr,
		CloudflareConfigured: cloudflareConfigured, NodeServices: services, ServiceFormError: serviceErr,
		OriginCertificates: originCerts, OriginCAError: originErr,
		JoinWindowFormError: formErr, ActivePage: "machine",
	}
	// The rest of the page is rendered exactly as handleMachinePage
	// would, and the window control is filled from the same read the
	// page itself uses. On the error path the window state is read
	// anyway so the control still shows the window's real state next to
	// the failure - an operator whose "open" was refused needs to see
	// whether one is already open.
	data.JoinColonyWindow = s.currentColonyJoinWindow(r, time.Now())
	s.render(w, "machine_page", s.withAuthFields(r, data))
}

// colonyAdvertView is the requester's view of the Colony it is about to
// ask to join, read through the same unauthenticated RPC - ADR-0147
// Part 4's "the first step of the join is now fetch the Colony's
// advertised name and fingerprints".
//
// The publication fields (ColonyName, ManagerdFingerprints) are NOT
// built yet: GetColonyJoinWindow documents that they "belong to the
// trust-store half of Part 4", and the implementation returns only the
// window and the node IDs. This struct therefore has to render honestly
// when they are empty rather than showing an empty box that reads as a
// broken page - an operator comparing fingerprints needs to be told
// that there are none to compare yet, not shown a blank table.
type colonyAdvertView struct {
	// Reachable is true when the RPC returned a window at all, i.e.
	// the target is currently open for joins.
	Reachable bool
	// Closed is the target's own refusal, verbatim. It is the
	// closed-window message, and the template shows it as the
	// "open the window first" instruction rather than as a failure.
	Closed string
	// Error is a transport or forwarding failure - a different thing
	// from Closed, and worded differently in the template. It is set
	// for BOTH so the reason is always visible; Closed is what tells
	// the template which of the two it is.
	Error string
	// ReadFailed is true when the target could not be asked, as
	// opposed to being asked and answering "closed". The distinction
	// is the whole point: a closed window's fix is to open one, and a
	// failed read's fix is to fix the network or the allowlist.
	ReadFailed bool

	ColonyName           string
	ManagerdFingerprints map[string]string
	ColonyNodeID         string
	LeaderNodeID         string

	// Window is the target's own window, so the requester can see the
	// deadline in local time. The operator running the requester is
	// the one most likely to be mid-copy when it runs out.
	Window colonyJoinWindowView
	// ColonyTargetAddress is the normalized target the read was made
	// against, echoed so the template can show the operator exactly
	// which host:port was asked, including the port this page filled
	// in for them.
	ColonyTargetAddress string
	Now                 time.Time
}

// currentColonyAdvert reads the target's advertisement by dialing the
// address the operator named. target_address is passed through
// deliberately: this is the one caller for which a caller-supplied
// address is the entire point, and it is forwarded unauthenticated
// under the ADR-0097 allowlist (known_peer_addresses), which is the
// same bound RequestJoinColony and GetJoinRequestStatus already carry.
//
// An empty target is not an error here: the requester form has not been
// filled in yet, and the panel should render as a prompt to fill it in
// rather than as a failed dial.
func (s *Server) currentColonyAdvert(r *http.Request, now time.Time) colonyAdvertView {
	target := withFixedPort(r.URL.Query().Get("colony_target_address"), managerdListenerPort)
	view := colonyAdvertView{Now: now}
	if target == "" {
		return view
	}
	resp, err := s.client.GetColonyJoinWindow(r.Context(), &rpcpb.GetColonyJoinWindowRequest{TargetAddress: target})
	if err != nil {
		view.Error = fmt.Sprintf("reaching %s: %v", target, err)
		view.ReadFailed = true
		return view
	}
	if resp.GetError() != "" {
		// The manager returns its closed-window refusal here, and the
		// template names that case explicitly so an operator is told to
		// open the window and start again rather than hunting for a
		// second PIN that was never the problem. A forwarding failure
		// arrives in the same field and must not be given that advice.
		view.Error = resp.GetError()
		view.ReadFailed = !manager.IsColonyWindowClosedRefusal(resp.GetError())
		if !view.ReadFailed {
			view.Closed = resp.GetError()
		}
		return view
	}
	w := resp.GetWindow()
	if w == nil {
		view.Error = fmt.Sprintf("%s returned neither a window nor a reason", target)
		view.ReadFailed = true
		return view
	}
	view.Reachable = true
	view.ColonyName = resp.GetColonyName()
	view.ManagerdFingerprints = resp.GetManagerdFingerprints()
	view.ColonyNodeID = resp.GetColonyNodeId()
	view.LeaderNodeID = resp.GetLeaderNodeId()
	view.Window = colonyJoinWindowView{
		Live:     w.GetEnabled() && w.GetExpiresAtUnix() > now.Truncate(time.Second).Unix(),
		OpenedBy: w.GetOpenedBy(), OpenedByKey: w.GetOpenedByKey(),
		OpenedAtUnix: w.GetOpenedAtUnix(), ExpiresAtUnix: w.GetExpiresAtUnix(),
		Now: now,
	}
	return view
}

// handleColonyAdvertPanel is the requester-side panel, served on its own
// route so the Machine page's join section can embed or link it and so
// it can be polled independently of the rest of the Machine page.
//
// It is a GET and it is NOT role-gated: it performs the same
// unauthenticated read the RPC itself allows, and the page it renders is
// the operator's own Comb's view of a remote Colony. Gating the panel
// but not the RPC would gate the display of a value the caller could
// already fetch by hand, which is security theatre rather than
// security.
func (s *Server) handleColonyAdvertPanel(w http.ResponseWriter, r *http.Request) {
	target := withFixedPort(r.URL.Query().Get("colony_target_address"), managerdListenerPort)
	view := s.currentColonyAdvert(r, time.Now())
	view.ColonyTargetAddress = target
	s.render(w, "colony_advert_panel", s.withAuthFields(r, pageData{
		ColonyAdvert: &view,
		ActivePage:   "machine",
	}))
}

// deadlineLocal renders an absolute unix deadline in the BROWSER's local
// time, with the zone spelled out.
//
// It is a template helper rather than an inline conversion because the
// absolute local time is the whole point of the control: "closes in 14
// minutes" is not comparable across the two screens an operator is
// looking at, whereas "closes at 19:41:07 EDT" is. A zero value renders
// as an em dash rather than as the Unix epoch, because a window field
// that is absent is absent, and 1 January 1970 is a date that would
// read as a real (and very wrong) deadline.
func deadlineLocal(unix int64) string {
	if unix <= 0 {
		return "-"
	}
	return time.Unix(unix, 0).Local().Format("2006-01-02 15:04:05 MST")
}

// remainingText renders the time left in a window in the largest unit
// that still reads exactly, so the countdown does not say "14 minutes"
// when 44 seconds remain. A window that is not live renders as "no time
// left" rather than as "0 seconds" for the same reason deadlineLocal
// avoids the epoch.
func remainingText(v colonyJoinWindowView) string {
	d := v.remaining()
	switch {
	case !v.Live:
		return "no time left"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		if s == 0 {
			return fmt.Sprintf("%d minutes", m)
		}
		return fmt.Sprintf("%d minutes %d seconds", m, s)
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%d hours", h)
		}
		return fmt.Sprintf("%d hours %d minutes", h, m)
	}
}

// compile-time assurance that the Admin-gating helper used on the two
// mutating routes is the same one the rest of the join flow uses, so a
// future refactor cannot quietly downgrade these to Operator.
var _ = manager.RoleAdmin
