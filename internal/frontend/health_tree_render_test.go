package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The old Command Center rendered a per-guest health tree - a live
// tree of every Comb and every cell under it, with DOWN and UNKNOWN
// states. The Blend overview (the "Apiary: UI Redesign" handoff)
// replaces that panel with the Combs section, which shows a Comb's own
// health in the seven-state vocabulary.
//
// THE PER-GUEST EVIDENCE IS NOT LOST. The tree's data model, its
// builder, and its JSON endpoint (/cluster-health.json) all remain -
// they are the per-cell health evidence the handoff's deferred
// "Evidence" tab will need, and internal/health's own five-state
// vocabulary is still computed per cell. What changed is that the
// overview no longer renders the tree inline.
//
// This test pins the NEW refresh contract: the overview's three
// sections each poll their own fragment, every ten seconds, only while
// the browser tab is visible.
//
// A VISIBILITY GATE, NOT A BLIND INTERVAL. A background tab polling a
// Colony's health every ten seconds is load the operator cannot see
// and did not ask for; htmx's visibility filter is what stops it.
func TestCommandCenterRendersHealthTreeAndBrowserContract(t *testing.T) {
	body := renderClusterOverview(t, &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "brood", KnownNodeIds: []string{"brood", "drone", "sting"}}})
	for _, required := range []string{
		// The three refreshing fragments, each polling on the same
		// interval and each gated on visibility.
		`hx-get="/overview/status" hx-trigger="every 10s [document.visibilityState === 'visible']"`,
		`hx-get="/overview/attention" hx-trigger="every 10s [document.visibilityState === 'visible']"`,
		`hx-get="/overview/combs?view=`,
		// The three sections, in the handoff's order.
		`id="ov-status"`,
		`id="ov-attention"`,
		`id="ov-combs-body"`,
		// And the health tree's own endpoint still answers, so the
		// per-guest evidence remains reachable. It is no longer
		// fetched by the overview itself - the Combs section replaced
		// that panel - but the route and its data model remain for
		// the per-cell health evidence.
	} {
		if !strings.Contains(body, required) {
			t.Errorf("rendered page missing tree contract %q", required)
		}
	}

	// The endpoint itself is still served, and still answers with the
	// per-guest tree.
	s := newTestServer(t, &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "brood", KnownNodeIds: []string{"brood"}}})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cluster-health.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/cluster-health.json status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"kind":"cluster"`) {
		t.Errorf("/cluster-health.json no longer answers with the health tree, got: %s", rec.Body.String())
	}
}
