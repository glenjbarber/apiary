package frontend

import (
	"net/http"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The HTTP surface of the Blend overview (handoff sections 7.5, 8, 11).
//
// The Comb view preference, the refreshing fragments, and the command
// palette all live here.

// combViewCookieName is the cookie that remembers the selected Comb
// view.
//
// WHY A COOKIE AND NOT A SERVER-SIDE PREFERENCE. The handoff is
// explicit: a per-user server-side preference would be replicated
// state, which is a schema change and a design decision for Glen, not
// part of this work. A cookie is ephemeral state that lives in the
// browser, so it is the right size for the problem.
const combViewCookieName = "apiary_combview"

// combViewCookieMaxAge is one year, in seconds.
const combViewCookieMaxAge = 365 * 24 * 60 * 60

// readCombView returns the Comb view this request asks for.
//
// The precedence is: an explicit ?combview= parameter, then the
// cookie, then Table.
//
// The parameter exists so the page works with JavaScript disabled: the
// segmented control's buttons are also plain links to
// "/?combview=cards", and a link that carries the choice in the URL
// works with no scripting at all.
//
// A missing or invalid value falls back to Table. It NEVER falls back
// to anything but Table, and it never returns an empty string - an
// empty view name would render a page with no Combs section at all,
// which looks like a Colony with no Combs.
func readCombView(r *http.Request) string {
	if v := r.URL.Query().Get("combview"); v != "" {
		return normalizeCombView(v)
	}
	if c, err := r.Cookie(combViewCookieName); err == nil {
		return normalizeCombView(c.Value)
	}
	return combViewTable
}

// setCombViewCookie records the selected Comb view in the browser.
//
// Path=/ so the preference applies to every page (the view is a
// property of the operator's session with the UI, not of one URL).
// SameSite=Lax and HttpOnly, and Secure whenever the page was served
// over TLS - the cookie carries no privilege, but it should not be a
// thing a cross-site request can set or read.
func setCombViewCookie(w http.ResponseWriter, r *http.Request, view string) {
	http.SetCookie(w, &http.Cookie{
		Name:     combViewCookieName,
		Value:    normalizeCombView(view),
		Path:     "/",
		MaxAge:   combViewCookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// handleOverviewCombs serves the Combs section as a fragment
// ("GET /overview/combs?view=table|cards|matrix").
//
// It is the target of the segmented control's hx-get, and it swaps
// ONLY the Combs section - the status line and the Needs-attention
// list have their own fragments.
//
// The response also sets the preference cookie, so a click through the
// button persists the choice for the next full page load.
//
// The response is a partial: it is NOT a full document. A fragment
// route that returned a whole page would replace the Combs section
// with a nested page.
func (s *Server) handleOverviewCombs(w http.ResponseWriter, r *http.Request) {
	view := normalizeCombView(r.URL.Query().Get("view"))
	if view == combViewTable {
		// A request that names no view is the initial paint's job,
		// not a switch. Reading the cookie here would mean clicking
		// "Table" on a page whose cookie says "cards" silently did
		// nothing - so an absent view falls back to the cookie, and
		// only an explicit one overrides it.
		view = readCombView(r)
	}
	setCombViewCookie(w, r, view)

	bundle := s.gatherOverviewForFragment(r)
	bundle.Combs.View = view
	s.render(w, "overview_combs", pageData{
		Overview:   bundle,
		CombView:   view,
		CanOperate: s.withAuthFieldsForFragment(r, pageData{}).CanOperate,
	})
}

// handleOverviewStatus serves the status line as a fragment
// ("GET /overview/status").
//
// It is polled every 10 seconds while the browser tab is visible. A
// failed refresh must leave the last content on screen and mark the
// values stale rather than blanking them - so on a failure this
// handler returns the LAST known content with the stale marker set,
// rather than an empty line or an error page.
func (s *Server) handleOverviewStatus(w http.ResponseWriter, r *http.Request) {
	bundle := s.gatherOverviewForFragment(r)
	s.render(w, "overview_status", pageData{Overview: bundle})
}

// handleOverviewAttention serves the Needs-attention list as a fragment
// ("GET /overview/attention").
func (s *Server) handleOverviewAttention(w http.ResponseWriter, r *http.Request) {
	bundle := s.gatherOverviewForFragment(r)
	s.render(w, "overview_attention", pageData{Overview: bundle})
}

// handlePalette serves the command palette's result list as an HTML
// fragment ("GET /palette?q=").
//
// It is triggered by the palette input's hx-get with a 150ms debounce.
//
// The ACTIONS group is server-rendered and ROLE-FILTERED: a Viewer sees
// no mutating actions, because the palette is a way to start an
// operation, and an action a session may not perform must not be
// offered at all - not merely refused when clicked.
//
// The "Go to" group searches across cells, combs, networks, images, and
// page names.
func (s *Server) handlePalette(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	pd := s.withAuthFieldsForFragment(r, pageData{})
	s.render(w, "palette_results", pageData{
		PaletteQuery: query,
		Palette:      s.paletteResults(r, query, pd.CanOperate, pd.CanAdmin),
	})
}

// gatherOverviewForFragment gathers the overview's evidence for a
// FRAGMENT route.
//
// A fragment has no page render of its own, so it has no Status
// response to hand over - it has to make its own. That is one extra
// RPC per refresh, and it is the price of the fragment: the full
// page already holds a Status response, but a fragment is a separate
// request with a separate context.
//
// It is ONE call, not one per Comb: gatherOverview takes the Status
// response and reuses it for every row, and the per-Comb evidence
// hangs off that single read.
func (s *Server) gatherOverviewForFragment(r *http.Request) overviewBundle {
	statusResp, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	return s.gatherOverview(r.Context(), r, statusResp, statusErr)
}
