package frontend

import (
	"strings"
	"testing"
)

// TestColonyTitle pins the top bar's Colony name.
//
// This is a regression guard for a real bug: the bar showed the raw
// raft Colony ID, a 36-character UUID, and the title was allowed to
// wrap anywhere, so it stacked a few characters per line down the
// page.
func TestColonyTitle(t *testing.T) {
	for _, tc := range []struct {
		colonyID, nodeID string
		name, full       string
	}{
		{
			colonyID: "e5130fb4-48bb-4283-af9f-35d68c680bb2", nodeID: "brood.lab3.apiary.work",
			name: "Colony e5130fb4", full: "e5130fb4-48bb-4283-af9f-35d68c680bb2",
		},
		{
			// No Colony identity yet: name the Comb that answered.
			colonyID: "", nodeID: "brood.lab3.apiary.work",
			name: "brood.lab3.apiary.work", full: "brood.lab3.apiary.work",
		},
		{
			// An ID with no hyphen is cut to 8 characters.
			colonyID: "0123456789abcdef", nodeID: "brood",
			name: "Colony 01234567", full: "0123456789abcdef",
		},
		{
			// Nothing at all: the template falls back to "Apiary".
			colonyID: "", nodeID: "",
			name: "", full: "",
		},
	} {
		name, full := colonyTitle(tc.colonyID, tc.nodeID)
		if name != tc.name || full != tc.full {
			t.Errorf("colonyTitle(%q, %q) = (%q, %q), want (%q, %q)", tc.colonyID, tc.nodeID, name, full, tc.name, tc.full)
		}
	}
}

// TestTopBarTitleStaysOnOneLine pins that the Colony name cannot wrap
// and carries its full identifier for hover.
func TestTopBarTitleStaysOnOneLine(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	i := strings.Index(body, ".topbar-title {")
	if i < 0 {
		t.Fatal("the page has no .topbar-title rule")
	}
	rule := body[i : i+strings.Index(body[i:], "}")]
	for _, want := range []string{"white-space:nowrap", "text-overflow:ellipsis", "min-width:9rem"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the .topbar-title rule is missing %q: %s", want, rule)
		}
	}
	if strings.Contains(rule, "overflow-wrap:anywhere") {
		t.Errorf("the .topbar-title rule still lets the title wrap anywhere: %s", rule)
	}
}
