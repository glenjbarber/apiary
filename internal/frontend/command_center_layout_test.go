package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The command center used to carry a second panel beside the colony
// status card: an "Attention" box whose whole body was the sentence
// "Use Failure preview and Why Not before reboot, migration, or
// teardown." plus two links that duplicated navigation already present
// in the sidebar and the top bar.
//
// The sentence was removed on purpose: it instructed the operator to
// consult two tools before acting, which reads as a warning that hangs
// over the colony status without saying anything about the colony's
// actual state. The "Preflight" step of the control-plane workflow
// panel, the Failure preview button in the top bar, and the Why Not
// and Recovery Handbook links in the sidebar all still carry that
// guidance.
//
// These tests pin the removal so the panel cannot quietly come back,
// and pin the things that replaced it so a future edit that removes the
// guidance *and* its replacement fails here rather than in review.

// TestCommandCenterHasNoAttentionPanel is the regression guard for the
// removal itself.
func TestCommandCenterHasNoAttentionPanel(t *testing.T) {
	body := renderClusterOverview(t, &fakeClient{})

	if strings.Contains(body, "Use Failure preview and Why Not before reboot, migration, or teardown.") {
		t.Error("command center still renders the removed Attention panel sentence")
	}
	if strings.Contains(body, "Ask Why Not?") {
		t.Error("command center still renders the removed Attention panel's Why Not button")
	}
	if strings.Contains(body, "Get the smallest blocker set") {
		t.Error("command center still renders the removed Attention panel's blocker-set caption")
	}
	if strings.Contains(body, "<h2>Attention</h2>") {
		t.Error("command center still renders the removed Attention panel heading")
	}
}

// TestCommandCenterKeepsPreflightGuidance checks that removing the
// panel did not remove the guidance with it. The sentence the Attention
// panel carried has an equivalent in the control-plane workflow panel's
// Preflight step, and the Failure preview entry point is still in the
// top bar.
func TestCommandCenterKeepsPreflightGuidance(t *testing.T) {
	body := renderClusterOverview(t, &fakeClient{})

	if !strings.Contains(body, "Use Failure preview or Why Not before a disruptive action.") {
		t.Error("command center lost the preflight guidance that the removed Attention panel carried")
	}
	if !strings.Contains(body, `href="/simulate"`) {
		t.Error("command center no longer offers Failure preview")
	}
	if !strings.Contains(body, `href="/why-not"`) {
		t.Error("command center no longer links to Why Not")
	}
}

// TestCommandCenterHealthCardSpansTopGrid checks the layout half of
// the change: with the second column panel gone, the colony status card
// must occupy the full width of the top grid rather than leaving an
// empty second column beside it.
func TestCommandCenterHealthCardSpansTopGrid(t *testing.T) {
	body := renderClusterOverview(t, &fakeClient{})

	if !strings.Contains(body, `class="panel health-card"`) {
		t.Fatal("command center does not render the health card at all")
	}
	if !strings.Contains(body, "Combs reachable") {
		t.Error("health card does not render its reachable count")
	}
}

// TestCommandCenterAttentionPanelIsNotRenderedOnAnyTopGridWidth guards
// the narrow-viewport rendering too: the removal is a template edit,
// not a CSS-only hiding, so the markup must be gone at every width.
func TestCommandCenterAttentionPanelIsNotRenderedOnAnyTopGridWidth(t *testing.T) {
	s := newTestServer(t, &fakeClient{})
	for _, path := range []string{"/", "/vms", "/simulate", "/why-not", "/recovery-handbook"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "Use Failure preview and Why Not before reboot") {
				t.Errorf("page %s still renders the removed Attention panel sentence", path)
			}
		})
	}
}
