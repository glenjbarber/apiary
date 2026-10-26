package frontend

import (
	"net/http"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The Blend landing page (handoff section 7): GET /.
//
// THE ORDER IS FIXED: status line, Needs attention, Combs. The
// handoff's acceptance criterion is that GET / shows those three, in
// that order.
//
// The page renders even when the evidence could not be read. An
// overview that says "the Colony could not be observed" is more useful
// than an error page, and the health chip is what tells the operator
// which of the two they are looking at.
func (s *Server) handleClusterOverviewPage(w http.ResponseWriter, r *http.Request) {
	view := readCombView(r)

	// The Status call is the page's own anchor. It is made once and
	// handed to every gather below, so the page costs one Status RPC
	// and not one per section.
	statusResp, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})

	pd := s.withAuthFieldsFrom(r, pageData{ActivePage: "stats"}, statusResp, statusErr)

	// The Comb view preference is recorded on every render, so the
	// choice made by a click (which sets the cookie) and the choice
	// made by a link (which carries ?combview=) both persist.
	setCombViewCookie(w, r, view)
	pd.CombView = view

	// The overview's own gathered evidence.
	bundle := s.gatherOverview(r.Context(), r, statusResp, statusErr)
	bundle.Combs.View = view
	pd.Overview = bundle

	// The rail. The Combs section's per-Comb tabs come from the
	// membership the page already read.
	combs := []string{}
	if statusResp != nil {
		combs = statusResp.GetKnownNodeIds()
	}
	alerts := navAlertsFrom(bundle.Combs, bundle.Attention)
	pd.Nav = s.buildNav(pd.ActivePage, "", pd.CanOperate, pd.CanAdmin, combs, alerts)

	// The Admin-only pending-join-requests panel, carried over from
	// the Command Center unchanged: it is a real form, and the
	// handoff did not ask for it to move.
	joinRequests := s.currentJoinRequests(r)
	pd.JoinRequests = joinRequests

	joinRequestError := r.URL.Query().Get("join_request_error")
	joinRequestErrorStage1Anchor := ""
	if joinRequestErrorPointsToStage1(joinRequestError) {
		if requestID := r.URL.Query().Get("join_request_stage1_request_id"); requestID != "" {
			joinRequestErrorStage1Anchor = "join-request-" + requestID + "-stage1"
		}
	}
	joinRequestAwaitingSecond := r.URL.Query().Get("join_request_awaiting_second")
	pd.JoinRequestError = joinRequestError
	pd.JoinRequestErrorPointsToStage1 = joinRequestErrorPointsToStage1(joinRequestError)
	pd.JoinRequestErrorStage1Anchor = joinRequestErrorStage1Anchor
	pd.JoinRequestAwaitingSecondAuthorization = joinRequestAwaitingSecond != ""
	pd.JoinRequestAwaitingSecondAuthorizationMessage = joinRequestAwaitingSecond
	pd.JoinRequestPreflightID = r.URL.Query().Get("preflight_request_id")
	pd.JoinRequestPreflightVerdict = r.URL.Query().Get("preflight_verdict")
	pd.JoinRequestPreflightDetail = r.URL.Query().Get("preflight_detail")

	// The page-level error, if the Colony could not be read at all.
	if statusErr != nil {
		pd.Error = statusErr.Error()
	}

	s.render(w, "cluster_overview_page", pd)
}
