package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// openingTagWithID returns the opening tag of the element carrying
// id="<id>" in body, or "" when there is none.
func openingTagWithID(body, id string) string {
	i := strings.Index(body, `id="`+id+`"`)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(body[:i], "<")
	end := strings.Index(body[i:], ">")
	if start < 0 || end < 0 {
		return ""
	}
	return body[start : i+end+1]
}

// TestOverviewPollersReplaceThemselves pins how each refreshing
// section is swapped.
//
// This is a regression guard for a real bug: every poller used
// hx-swap="innerHTML" while its route returned the whole section,
// wrapper and heading included. Each refresh therefore put a new copy
// of the section INSIDE the old one: a second "Needs attention"
// heading inside the box, a second padded status line, and a second
// Combs heading and view switch. A poller must replace itself, and
// where its route returns more than the poller, hx-select must pick
// the poller back out by id.
func TestOverviewPollersReplaceThemselves(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	for _, tc := range []struct {
		id  string
		sel string
	}{
		{id: "ov-status"},
		{id: "ov-attention", sel: "#ov-attention"},
		{id: "ov-combs-body", sel: "#ov-combs-body"},
	} {
		tag := openingTagWithID(body, tc.id)
		if tag == "" {
			t.Errorf("the overview has no element with id %q", tc.id)
			continue
		}
		if !strings.Contains(tag, `hx-swap="outerHTML"`) {
			t.Errorf("#%s does not replace itself on refresh: %s", tc.id, tag)
		}
		if strings.Contains(tag, `hx-swap="innerHTML"`) {
			t.Errorf("#%s fills itself on refresh, which nests a copy of the section inside it: %s", tc.id, tag)
		}
		if tc.sel != "" && !strings.Contains(tag, `hx-select="`+tc.sel+`"`) {
			t.Errorf("#%s does not select itself out of the route's response: %s", tc.id, tag)
		}
	}

	// The view switch replaces the whole Combs section, so the pressed
	// button and the body change together.
	for _, v := range []string{"table", "cards", "matrix"} {
		want := `hx-get="/overview/combs?view=` + v + `" hx-target="#ov-combs" hx-select="#ov-combs" hx-swap="outerHTML"`
		if !strings.Contains(body, want) {
			t.Errorf("the %q view button does not replace the whole Combs section", v)
		}
	}
	if strings.Contains(body, `hx-target="#ov-combs-body"`) {
		t.Error("a view button still fills the Combs body, which nests a second heading and view switch inside it")
	}
}

// TestOverviewFragmentsCarryTheirSwapTarget pins that each fragment
// route returns exactly one element with the id its poller selects, so
// an outerHTML swap replaces the poller with one copy of itself.
func TestOverviewFragmentsCarryTheirSwapTarget(t *testing.T) {
	for _, tc := range []struct {
		path string
		ids  []string
	}{
		{"/overview/status", []string{"ov-status"}},
		{"/overview/attention", []string{"ov-attention"}},
		{"/overview/combs?view=table", []string{"ov-combs", "ov-combs-body"}},
		{"/overview/combs?view=cards", []string{"ov-combs", "ov-combs-body"}},
		{"/overview/combs?view=matrix", []string{"ov-combs", "ov-combs-body"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newTestServer(t, overviewTestClient()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, id := range tc.ids {
				if n := strings.Count(body, `id="`+id+`"`); n != 1 {
					t.Errorf("the fragment has %d elements with id %q, want exactly 1", n, id)
				}
			}
		})
	}

	// The status route returns the poller itself, with no wrapper, so
	// a plain outerHTML swap is enough.
	rec := httptest.NewRecorder()
	newTestServer(t, overviewTestClient()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/overview/status", nil))
	if body := strings.TrimSpace(rec.Body.String()); !strings.HasPrefix(body, `<div class="ov-status`) {
		t.Errorf("the status fragment's root is not the status line itself: %.80s", body)
	}
}
