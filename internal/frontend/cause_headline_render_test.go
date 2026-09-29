package frontend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// This file covers the per-Comb cause line on the command-center node
// status card - the line under a node's badge row that reads
//
//	evidence current   reconciler_last_tick: the last reconcile tick succeeded   full evidence
//
// The property under test is that the VERDICT is spoken exactly once on
// that line. The verdict and the cause are different layers with
// different jobs: combCauseBadge owns which state may look green, and
// the headline beside it names the source of that verdict in prose. When
// the headline restated the badge, every Comb rendered its verdict twice
// - "observed healthy observed healthy" - which reads as two findings
// and is one.
//
// Separately, when this cause badge would ALSO restate the card's own
// HealthStatus badge one row up - both fresh, non-stale, and both
// spelling "healthy" - clusterNodeEvidence relabels this one badge to
// "evidence current" instead of combCauseBadge's own "observed healthy".
// Same state, same green, same finding - only the word choice changes,
// specifically to stop a healthy Comb's card from printing "healthy"
// twice in two different rows for what reads as the same reason.

// nodeCauseLine returns the visible text of one node's cause line, with
// tags stripped and runs of whitespace collapsed, so an assertion is
// made against what an operator actually reads rather than markup.
//
// Scoping to one card matters: the page renders several Combs, and the
// same badge vocabulary also appears in the colony header above them, so
// a document-wide search would match the wrong text.
func nodeCauseLine(t *testing.T, body, nodeID string) string {
	t.Helper()
	const card = `class="cockpit-topology-node`
	cardHTML := ""
	for at := 0; ; {
		i := strings.Index(body[at:], card)
		if i < 0 {
			t.Fatalf("no topology card for %q in the rendered page", nodeID)
		}
		i += at
		at = i + len(card)
		// The card ends where the next one begins. The cause line is the
		// last child div, so a slice to the next card is enough and does
		// not depend on the markup's div nesting.
		end := len(body)
		if j := strings.Index(body[at:], card); j >= 0 {
			end = at + j
		}
		if segment := body[i:end]; strings.Contains(segment, `href="/host/`+nodeID+`"`) {
			cardHTML = segment
			break
		}
	}
	const cause = `class="cockpit-node-cause"`
	c := strings.Index(cardHTML, cause)
	if c < 0 {
		t.Fatalf("card for %q has no cause line: %s", nodeID, cardHTML)
	}
	c += len(cause)
	// Step past the rest of the opening tag, so the data-cause-state
	// attribute value is not read as prose.
	if g := strings.Index(cardHTML[c:], ">"); g >= 0 {
		c += g + 1
	}
	e := strings.Index(cardHTML[c:], "</div>")
	if e < 0 {
		t.Fatalf("cause line for %q is not terminated by </div>: %s", nodeID, cardHTML)
	}
	return strings.Join(strings.Fields(stripTags(cardHTML[c:c+e])), " ")
}

// stripTags removes HTML tags from a rendered fragment and decodes the
// entities the templates actually produce, so the result is the text an
// operator reads rather than markup. Only the entities this page's
// templates emit are handled; anything else is left as it is rather than
// guessed at.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	out := b.String()
	for _, sub := range [][2]string{
		{"&#39;", "'"}, {"&middot;", "·"}, {"&amp;", "&"},
		{"&quot;", `"`}, {"&lt;", "<"}, {"&gt;", ">"},
	} {
		out = strings.ReplaceAll(out, sub[0], sub[1])
	}
	return out
}

// TestNodeStatusCauseLine_SpeaksItsVerdictOnce is the reproduction of the
// sting.lab3.home.arpa report. The Comb is reachable, its health is
// healthy, its own last reconcile tick succeeded recently, and it agrees
// with the rest of the Colony's FSM state digest (ADR-0143). All four of
// those are separate, honest signals and every one of them must survive
// the fix; what must not happen is the cause verdict being spelled out
// twice on the same line, or the cause badge restating "healthy" a
// second time right under the badge row that already says it.
func TestNodeStatusCauseLine_SpeaksItsVerdictOnce(t *testing.T) {
	now := time.Now()
	const digest = "aaaa"
	const node = "sting.lab3.home.arpa"
	const peer = "drone.lab3.home.arpa"

	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: node, RaftReachable: true, RaftState: "Follower",
			RaftNodeId: node, RaftLeaderId: peer,
			RaftAppliedIndex: 181, RaftStateDigest: digest, RaftLastLogIndex: 181,
			KnownNodeIds: []string{node, peer},
			Members: []*rpcpb.RaftMember{
				{NodeId: node, Suffrage: "Voter"},
				{NodeId: peer, Suffrage: "Voter"},
			},
		},
		hostStatsResp: tickingCombHostStats(node, now),
	}
	peers := &fakePeerHostStatsClient{
		resp: tickingCombHostStats(peer, now),
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: peer, RaftReachable: true, RaftState: "Follower",
			RaftNodeId: peer, RaftLeaderId: node,
			RaftAppliedIndex: 181, RaftStateDigest: digest, RaftLastLogIndex: 181,
			KnownNodeIds: []string{node, peer},
		},
	}

	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The badge row must keep every one of its own separate signals. The
	// digest verdict in particular is a DIFFERENT fact from the cause
	// verdict, and de-duplicating the cause line is not a licence to drop
	// it.
	for _, want := range []string{"Reachable", "healthy", "State matches"} {
		if !strings.Contains(body, want) {
			t.Fatalf("badge row lost %q, so this test is not exercising the reported page: %s", want, colonyExcerpt(body))
		}
	}

	got := nodeCauseLine(t, body, node)
	want := "evidence current reconciler_last_tick: the last reconcile tick succeeded full evidence"
	if got != want {
		t.Errorf("cause line for a healthy Comb whose tick succeeded:\n got: %q\nwant: %q", got, want)
	}
	// Belt and braces, and the assertion that names the defect this test
	// guards against: the cause line must never say "healthy" itself when
	// the badge row one line up already said it - that word appearing
	// twice for the same Comb is exactly the duplication this relabeling
	// exists to avoid, even though both rows would be equally correct on
	// their own.
	if strings.Contains(got, "healthy") {
		t.Errorf("cause line restates \"healthy\" from the badge row above it: %q", got)
	}
}

// TestNodeStatusCauseLine_UnobservedCombStaysUnobserved guards the
// property a de-duplication is most likely to break. A Comb nothing
// could be observed about must keep saying so, exactly once, and must
// never be rendered healthy by a fix that only removed prose.
func TestNodeStatusCauseLine_UnobservedCombStaysUnobserved(t *testing.T) {
	const node = "sting.lab3.home.arpa"
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: node, RaftReachable: true, RaftState: "Follower",
			RaftNodeId: node, RaftLeaderId: node, RaftAppliedIndex: 181,
			RaftLastLogIndex: 181,
			KnownNodeIds:     []string{node, "drone.lab3.home.arpa"},
			Members:          []*rpcpb.RaftMember{{NodeId: node, Suffrage: "Voter"}},
		},
		// No reconcile history at all: nothing about this Comb's
		// reconciler could be read.
		hostStatsResp: &rpcpb.HostStatsResponse{NodeId: node},
	}
	// No peer forwarding: nothing about any other Comb can be observed.
	peers := &fakePeerHostStatsClient{err: context.DeadlineExceeded}

	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	line := nodeCauseLine(t, body, node)
	if n := strings.Count(line, "not observed"); n != 1 {
		t.Errorf("cause line states its verdict %d times, want 1: %q", n, line)
	}
	if strings.Contains(line, "observed healthy") {
		t.Errorf("an unobservable Comb rendered as healthy: %q", line)
	}
	if !strings.Contains(line, "not applicable") && !strings.Contains(line, "not observed") {
		t.Errorf("cause line does not name the verdict a Comb with no evidence deserves: %q", line)
	}
}

// TestNodeStatusCauseLine_StaleSuccessIsNotRestatedAsGreen covers the
// staleness half of the same layering. The badge is what may look
// green, and it says "observed healthy (stale)" when the success is
// past the Comb's own freshness limit. The headline beside it names the
// age, once.
func TestNodeStatusCauseLine_StaleSuccessIsNotRestatedAsGreen(t *testing.T) {
	now := time.Now()
	const node = "sting.lab3.home.arpa"

	// The interval is 300s, so the freshness limit is 3x300s = 15m
	// (health.reconcileFreshnessMultiplier). A success 30 minutes old
	// is well past it.
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: node, RaftReachable: true, RaftState: "Follower",
			RaftNodeId: node, RaftLeaderId: node, RaftAppliedIndex: 181,
			RaftLastLogIndex: 181,
			KnownNodeIds:     []string{node, "drone.lab3.home.arpa"},
			Members: []*rpcpb.RaftMember{
				{NodeId: node, Suffrage: "Voter"},
				{NodeId: "drone.lab3.home.arpa", Suffrage: "Voter"},
			},
		},
		hostStatsResp: &rpcpb.HostStatsResponse{
			NodeId:                   node,
			ReconcileIntervalSeconds: 300,
			LastReconcileAttemptUnix: now.Add(-30 * time.Minute).Unix(),
			LastReconcileSuccessUnix: now.Add(-30 * time.Minute).Unix(),
		},
	}
	peers := &fakePeerHostStatsClient{err: context.DeadlineExceeded}

	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	line := nodeCauseLine(t, rec.Body.String(), node)

	if n := strings.Count(line, "observed healthy (stale)"); n != 1 {
		t.Errorf("cause line names the stale verdict %d times, want 1: %q", n, line)
	}
	if !strings.Contains(line, "past this Comb's freshness limit") {
		t.Errorf("stale cause line does not say the success is old: %q", line)
	}
}

// tickingCombHostStats is one Comb's own reconcile history with a tick
// that just succeeded, which is what a working Comb looks like.
func tickingCombHostStats(node string, now time.Time) *rpcpb.HostStatsResponse {
	return &rpcpb.HostStatsResponse{
		NodeId:                   node,
		ReconcileIntervalSeconds: 300,
		LastReconcileAttemptUnix: now.Add(-10 * time.Second).Unix(),
		LastReconcileSuccessUnix: now.Add(-10 * time.Second).Unix(),
	}
}
