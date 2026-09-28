package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/statedigest"
)

// These check that ADR-0143's badge actually reaches the rendered HTML,
// and - the part that matters - what it renders when the evidence is
// thin. The verdict logic is covered by statedigest_test.go; these exist
// because a correct verdict that never reaches the page is not a badge.

func renderClusterOverview(t *testing.T, client *fakeClient) string {
	t.Helper()
	s := newTestServer(t, client)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// TestStateDigestBadgeRendersUnobservedWhenNothingWasRead is the case a
// naive implementation gets wrong at the template level. The test server
// has no peer forwarding, so only the answering node could report a
// digest; with none reported, the badge must say so in words. A missing
// badge, or a green one, would both be lies.
func TestStateDigestBadgeRendersUnobservedWhenNothingWasRead(t *testing.T) {
	body := renderClusterOverview(t, &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId:    "brood.lab3.home.arpa",
		RaftReachable:    true,
		RaftState:        "Follower",
		RaftNodeId:       "brood.lab3.home.arpa",
		RaftLeaderId:     "drone.lab3.home.arpa",
		RaftAppliedIndex: 181,
		KnownNodeIds:     []string{"brood.lab3.home.arpa", "drone.lab3.home.arpa"},
	}})

	if !strings.Contains(body, "State unobserved") {
		t.Errorf("page has no State unobserved badge, so a digest nobody read is being rendered as agreement")
	}
	if !strings.Contains(body, "no Comb reported an FSM state digest") {
		t.Errorf("page does not carry the colony-wide explanation, got: %s", colonyExcerpt(body))
	}
	if strings.Contains(body, "State matches") || strings.Contains(body, colonyAgreedLabel) {
		t.Error("page claims state agreement from no observation at all")
	}
}

// TestStateDigestBadgeRendersUnobservedFromASingleReading is the same
// guard one step further along: exactly one Comb reported a digest, which
// trivially "agrees" with itself. The page must still refuse to call that
// agreement, because the colony's other Combs were never heard from.
func TestStateDigestBadgeRendersUnobservedFromASingleReading(t *testing.T) {
	body := renderClusterOverview(t, &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId:    "brood.lab3.home.arpa",
		RaftReachable:    true,
		RaftState:        "Follower",
		RaftNodeId:       "brood.lab3.home.arpa",
		RaftLeaderId:     "drone.lab3.home.arpa",
		RaftAppliedIndex: 181,
		RaftStateDigest:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		KnownNodeIds:     []string{"brood.lab3.home.arpa", "drone.lab3.home.arpa"},
	}})

	if !strings.Contains(body, "State unobserved") {
		t.Error("page does not mark the single observed reading as uncomparable")
	}
	if !strings.Contains(body, "a single voter always agrees with itself") {
		t.Errorf("page does not explain why one reading proves nothing, got: %s", colonyExcerpt(body))
	}
	if strings.Contains(body, colonyAgreedLabel) {
		t.Error("page claims colony-wide agreement from exactly one observation")
	}
}

// TestStateDigestBadgeCarriesItsExplanation checks the operator can find
// out why a row is bad without leaving the page: the verdict sentence is
// rendered as the badge's title, not just computed and discarded.
func TestStateDigestBadgeCarriesItsExplanation(t *testing.T) {
	body := renderClusterOverview(t, &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId:    "brood.lab3.home.arpa",
		RaftReachable:    true,
		RaftState:        "Follower",
		RaftNodeId:       "brood.lab3.home.arpa",
		RaftLeaderId:     "drone.lab3.home.arpa",
		RaftAppliedIndex: 181,
		KnownNodeIds:     []string{"brood.lab3.home.arpa"},
	}})

	// Matched without the leading "this Comb's", because html/template
	// escapes the apostrophe to &#39; inside the attribute.
	if !strings.Contains(body, `title="FSM state digest could not be read`) &&
		!strings.Contains(body, "FSM state digest could not be read") {
		t.Errorf("per-row badge carries no explanation, got: %s", colonyExcerpt(body))
	}
}

// TestStateDigestBadgeRendersProvenDivergence closes the gap the
// HTTP-level tests above cannot: this test server has no peer
// forwarding, so no two differing digests can arrive through a real
// request. It renders the template directly with the node set the
// colony-wide comparison produced, which is the one path where the
// diverged badge - the badge that matters - reaches real HTML.
func TestStateDigestBadgeRendersProvenDivergence(t *testing.T) {
	nodes := []clusterNodeView{
		{NodeID: "brood.lab3.home.arpa", Reachable: true, HealthStatus: "healthy", StateDigest: "aaaa", AppliedIndex: 181},
		{NodeID: "drone.lab3.home.arpa", Reachable: true, HealthStatus: "healthy", StateDigest: "aaaa", AppliedIndex: 181},
		{NodeID: "buzz.lab3.home.arpa", Reachable: true, HealthStatus: "healthy", StateDigest: "aaaa", AppliedIndex: 181},
		{NodeID: "sting.lab3.home.arpa", Reachable: true, HealthStatus: "healthy", StateDigest: "bbbb", AppliedIndex: 181},
	}
	views, colony := stateDigestVerdicts([]statedigest.Observation{
		renderObservation("brood.lab3.home.arpa", "aaaa", 181),
		renderObservation("drone.lab3.home.arpa", "aaaa", 181),
		renderObservation("buzz.lab3.home.arpa", "aaaa", 181),
		renderObservation("sting.lab3.home.arpa", "bbbb", 181),
	})
	for i := range nodes {
		view := views[nodes[i].NodeID]
		nodes[i].DigestState = view.State
		nodes[i].DigestBadgeClass = view.BadgeClass
		nodes[i].DigestBadgeLabel = view.Label
		nodes[i].DigestDetail = view.Detail
	}

	s := newTestServer(t, &fakeClient{})
	rec := httptest.NewRecorder()
	s.render(rec, "cluster_overview_page", pageData{ClusterNodes: nodes, StateDigestColony: colony, ActivePage: "stats"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "State differs") {
		t.Errorf("page has no State differs badge for a proven divergence, got: %s", colonyExcerpt(body))
	}
	if strings.Contains(body, "State matches") {
		t.Error("page shows a match badge on a colony whose state machines have diverged")
	}
	if !strings.Contains(body, colonyDivergedLabel) {
		t.Errorf("page lacks the colony-level %q badge, got: %s", colonyDivergedLabel, colonyExcerpt(body))
	}
	// The rendered page must still carry the limitation, not just the
	// alarm: an operator reading this must not conclude a named Comb is
	// at fault.
	if !strings.Contains(body, "does not say which side is wrong") {
		t.Error("page renders the divergence without stating that the digest cannot identify the wrong side")
	}
}

// colonyExcerpt returns the topology panel's digest sentence, or a short
// marker when it is absent, so a failure message shows what was rendered
// without dumping a whole page.
func colonyExcerpt(body string) string {
	const marker = "FSM state agreement (ADR-0143): "
	if i := strings.Index(body, marker); i >= 0 {
		rest := body[i+len(marker):]
		if j := strings.Index(rest, "</p>"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return "(no digest sentence rendered)"
}
