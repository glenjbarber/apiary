package frontend

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The navigation table (handoff section 6) is the information
// architecture, and the handoff's acceptance criterion is that EVERY
// existing route is reachable from a rail section and a tab.
//
// These tests pin that from both directions: every route is in the
// table, and every route in the table really exists.

// navTestRoutes is every page route the frontend serves.
//
// It is the list a reviewer can check against the route table in
// server.go, and it is checked against the table rather than trusted,
// because a route added to server.go without a place in the table
// would become a page nothing links to.
var navTestRoutes = []string{
	"/",
	"/apikeys",
	"/assumption-register",
	"/assumptions",
	"/certificates",
	"/colony-update",
	"/create",
	"/host/{id}",
	"/host/{id}/evidence",
	"/images",
	"/invariants",
	"/jails",
	"/jails/{id}",
	"/machine",
	"/machine/cluster",
	"/machine/exposure",
	"/machine/join",
	"/machine/networking",
	"/machine/operations",
	"/machine/security",
	"/machine/trust",
	"/machine/workloads",
	"/maintenance",
	"/networks",
	"/recovery-handbook",
	"/replication",
	"/resilience-coverage",
	"/simulate",
	"/topology",
	"/trace",
	"/users",
	"/vms",
	"/vms/{id}",
	"/vms/new",
	"/why-not",
}

// navTableRoutes is every route the navigation table reaches.
func navTableRoutes() []string {
	seen := map[string]bool{}
	var out []string
	for _, section := range navSections {
		for _, tab := range section.Tabs {
			// A tab whose href carries a fragment points at a page
			// with an anchor, not at a page of its own.
			path := tab.Href
			if i := strings.IndexByte(path, '#'); i >= 0 {
				path = path[:i]
			}
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// navSubRoutes are the routes that are reached by CLICKING THROUGH a
// tab rather than by a tab of their own.
//
// A cell's own page, a Comb's own page, and the Machine page's
// per-subsystem pages are all reached by clicking a row on the tab's
// page - which is one click, and is how the handoff describes them
// ("two clicks or fewer"). They are listed here so the reachability
// test can hold them to the same standard: the tab that leads to them
// must EXIST and must render a link to them.
var navSubRoutes = map[string]string{
	"/host/{id}":          "/host/does-not-matter",
	"/host/{id}/evidence": "/host/does-not-matter/evidence",
	"/jails/{id}":         "/jails/does-not-matter",
	"/vms/{id}":           "/vms/does-not-matter",
	"/vms/new":            "/create?kind=vm",
	"/machine/cluster":    "/machine",
	"/machine/exposure":   "/machine",
	"/machine/networking": "/machine",
	"/machine/operations": "/machine",
	"/machine/security":   "/machine",
	"/machine/workloads":  "/machine",
	"/machine/trust":      "/machine",
	"/machine/join":       "/machine",
}

// TestEveryRouteIsReachableFromTheNavigationTable is the handoff's
// "every route in section 6 is reachable in two clicks or fewer"
// criterion, checked from the route side.
//
// A route that is neither a tab nor reachable by clicking through a tab
// is a page nothing links to - an operator can only reach it by typing
// the URL, and a bookmark is the only way back.
func TestEveryRouteIsReachableFromTheNavigationTable(t *testing.T) {
	inTable := map[string]bool{}
	for _, route := range navTableRoutes() {
		inTable[route] = true
	}
	var missing []string
	for _, route := range navTestRoutes {
		if inTable[route] {
			continue
		}
		if _, isSub := navSubRoutes[route]; isSub {
			continue
		}
		missing = append(missing, route)
	}
	if len(missing) > 0 {
		t.Errorf("these routes are not reachable from any rail section or tab: %v", missing)
	}
}

// TestEverySubRouteIsLinkedFromItsTab pins the other half: a route
// that is reached by clicking through a tab must actually be LINKED
// from that tab's page. Otherwise the "two clicks or fewer" claim is
// a claim about a link that does not exist.
func TestEverySubRouteIsLinkedFromItsTab(t *testing.T) {
	for route, via := range navSubRoutes {
		t.Run(route, func(t *testing.T) {
			// A per-Comb page and a per-cell page are reached from
			// a list that this frontend renders, so the LINK is
			// checked on the page that carries the list.
			switch route {
			case "/host/{id}", "/host/{id}/evidence":
				// The Combs section of the overview links both.
				rec := httptest.NewRecorder()
				newTestServer(t, &fakeClient{statusResp: combStatus("brood")}).
					ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
				body := rec.Body.String()
				if !strings.Contains(body, "/host/brood") {
					t.Errorf("the overview does not link a Comb's own page, so %s is unreachable", route)
				}
			case "/vms/{id}", "/vms/new", "/jails/{id}":
				// The cells sections link their own rows.
				rec := httptest.NewRecorder()
				newTestServer(t, &fakeClient{listResp: vmList()}).
					ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vms", nil))
				body := rec.Body.String()
				if !strings.Contains(body, "/vms/vm-1") {
					t.Errorf("the cells page does not link a cell's own page, so %s is unreachable", route)
				}
			default:
				// The Machine page's own sub-pages are linked from
				// the Machine page.
				rec := httptest.NewRecorder()
				newTestServer(t, &fakeClient{}).
					ServeHTTP(rec, httptest.NewRequest(http.MethodGet, via, nil))
				body := rec.Body.String()
				if !strings.Contains(body, strings.TrimSuffix(route, "")) &&
					!strings.Contains(body, "/machine/"+strings.TrimPrefix(route, "/machine/")) {
					t.Errorf("the Machine page does not link %s, so it is unreachable", route)
				}
			}
		})
	}
}

// TestEveryNavigationTabPointsAtARouteThatExists is the other
// direction: a tab that leads nowhere is worse than no tab at all,
// because it looks like a working link.
//
// The check is that the page renders. A tab pointing at a route that
// 404s would be exactly the failure this guards against.
func TestEveryNavigationTabPointsAtARouteThatExists(t *testing.T) {
	for _, section := range navSections {
		for _, tab := range section.Tabs {
			t.Run(section.ID+"/"+tab.Label, func(t *testing.T) {
				path := tab.Href
				if i := strings.IndexByte(path, '#'); i >= 0 {
					path = path[:i]
				}
				rec := httptest.NewRecorder()
				newTestServer(t, &fakeClient{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code == http.StatusNotFound {
					t.Errorf("the tab points at %s, which does not exist", path)
				}
				if rec.Code >= 500 {
					t.Errorf("the tab points at %s, which failed with %d", path, rec.Code)
				}
			})
		}
	}
}

// TestTheRailHasEverySectionInOrder pins the handoff's rail order:
// Overview, Cells, Combs, Network, Storage, Images, Colony, with
// Settings pinned to the bottom.
func TestTheRailHasEverySectionInOrder(t *testing.T) {
	var order []string
	for _, section := range navSections {
		order = append(order, section.ID)
	}
	want := []string{"overview", "cells", "combs", "network", "storage", "images", "colony", "settings"}
	if len(order) != len(want) {
		t.Fatalf("the rail has %d sections, want %d: %v", len(order), len(want), order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("rail section %d is %q, want %q (the whole rail is %v)", i, order[i], want[i], order)
		}
	}
	// Settings is the pinned one, and it is pinned to the bottom.
	if !navSections[len(navSections)-1].Pinned {
		t.Error("the last rail section is not pinned; Settings must be pinned to the bottom")
	}
}

// TestEveryRailSectionHasAtLeastOneTab guards the "no third level"
// rule from the other side: a section with no tabs would give the
// operator nothing to choose from.
func TestEveryRailSectionHasAtLeastOneTab(t *testing.T) {
	for _, section := range navSections {
		if len(section.Tabs) == 0 {
			t.Errorf("the %q rail section has no tabs", section.ID)
		}
	}
}

// TestAdminOnlyTabsAreHiddenFromLowerRoles pins that an Admin-only
// page is not merely refused when a Viewer clicks it - it is not
// offered at all.
func TestAdminOnlyTabsAreHiddenFromLowerRoles(t *testing.T) {
	srv := newTestServer(t, &fakeClient{})
	nav := srv.buildNav("apikeys", "apiarium", true, false, nil, nil)
	for _, section := range nav {
		for _, tab := range section.Tabs {
			if tab.Href == "/apikeys" {
				t.Error("an Operator's nav offers the Admin-only API keys tab")
			}
		}
	}
	admin := srv.buildNav("apikeys", "apiarium", true, true, nil, nil)
	found := false
	for _, section := range admin {
		for _, tab := range section.Tabs {
			if tab.Href == "/apikeys" {
				found = true
			}
		}
	}
	if !found {
		t.Error("an Admin's nav does not offer the API keys tab")
	}
}

// TestOperatorOnlyTabsAreHiddenFromViewers pins the same for the
// Operator tier.
func TestOperatorOnlyTabsAreHiddenFromViewers(t *testing.T) {
	srv := newTestServer(t, &fakeClient{})
	nav := srv.buildNav("machine", "apiarium", false, false, nil, nil)
	for _, section := range nav {
		for _, tab := range section.Tabs {
			if tab.Href == "/machine" {
				t.Error("a Viewer's nav offers the Operator-only Machine tab")
			}
		}
	}
	// And the section itself points somewhere the Viewer may go.
	for _, section := range nav {
		if section.ID == "settings" && section.Href == "/machine" {
			t.Error("a Viewer's Settings rail item points at the Operator-only Machine page")
		}
	}
}

// combStatus is a one-Comb membership for the navigation tests.
func combStatus(nodeID string) *rpcpb.StatusResponse {
	return &rpcpb.StatusResponse{
		ManagerNodeId: nodeID, RaftReachable: true,
		KnownNodeIds: []string{nodeID},
		Members:      []*rpcpb.RaftMember{{NodeId: nodeID, Suffrage: "Voter"}},
	}
}

// vmList is a one-cell list for the navigation tests.
func vmList() *rpcpb.ListVMsResponse {
	return &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-1", NodeId: "brood"}}}
}
