package frontend

// ADR-0177's trust store page.
//
// What these pin: that the store is rendered, that a STAGED
// certificate is shown beside the current one (the case that matters
// most - the Colony trusting two certificates for one Comb), and that
// the three acts are separate and separately reasoned.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// A staged certificate is shown beside the current one.
//
// This is the case that matters most: between staging and activation
// the Colony is trusting TWO certificates for one Comb, and an
// operator who cannot see that cannot reason about it.
func TestTrustStorePage_ShowsAStagedCertificateBesideTheCurrentOne(t *testing.T) {
	client := &fakeClient{
		trustedPeersResp: &rpcpb.ListTrustedPeersResponse{Peers: []*rpcpb.TrustedPeer{
			{
				NodeId:       "node02",
				CombName:     "drone",
				Fingerprint:  "SHA256:AA:BB:CC",
				CertPem:      "cert",
				NotAfterUnix: 1800000000,
				IsVoter:      true,
				Next: &rpcpb.TrustedPeer{
					NodeId:       "node02",
					Fingerprint:  "SHA256:DD:EE:FF",
					CertPem:      "next cert",
					NotAfterUnix: 1800000000,
				},
			},
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine/trust", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"SHA256:AA:BB:CC", // the current certificate
		"SHA256:DD:EE:FF", // the staged one
		"Activate",        // and the act that promotes it
		"Abandon",         // and the act that drops it
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
	// A Comb with a staged certificate offers activation and
	// abandonment, and NOT staging - staging again would be a second
	// transition on a Comb that already has one.
	if strings.Contains(body, `value="stage"`) {
		t.Error("the page offers to stage a certificate for a Comb that already has one staged")
	}
	if !strings.Contains(body, `value="activate"`) || !strings.Contains(body, `value="abandon"`) {
		t.Error("the page does not offer to activate or abandon the staged certificate")
	}
}

// An empty store says so, and says WHY.
//
// "No pinned members" and "the read failed" are different facts, and
// only one of them is fixed by pinning certificates.
func TestTrustStorePage_AnEmptyStoreExplainsItself(t *testing.T) {
	s := newTestServer(t, &fakeClient{trustedPeersResp: &rpcpb.ListTrustedPeersResponse{}})

	req := httptest.NewRequest(http.MethodGet, "/machine/trust", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no pinned members") {
		t.Errorf("the page does not say the store is empty: %s", body)
	}
	if !strings.Contains(body, "pin-trusted-peers") && !strings.Contains(body, "before the peer trust store existed") {
		t.Error("the page does not say how the store is populated")
	}
}

// Every write requires a reason.
//
// Replacing a Comb's certificate changes what the Colony trusts, and an
// incident review can only answer "why did this identity change" from
// the log.
func TestTrustTransition_RequiresAReason(t *testing.T) {
	s := newTestServer(t, &fakeClient{})

	req := httptest.NewRequest(http.MethodPost, "/machine/trust/transition", strings.NewReader("node_id=node02&action=activate"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "reason is required") {
		t.Errorf("a transition with no reason was not refused by name: %s", rec.Body.String())
	}
}

// An unknown action is refused rather than ignored.
func TestTrustTransition_RefusesAnUnknownAction(t *testing.T) {
	s := newTestServer(t, &fakeClient{})

	req := httptest.NewRequest(http.MethodPost, "/machine/trust/transition",
		strings.NewReader("node_id=node02&action=obliterate&reason=because"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "unknown action") {
		t.Errorf("an unknown action was not refused by name: %s", rec.Body.String())
	}
}
