package frontend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/hostpkg"
)

// stubHostPkgSource is a scripted hostPkgSource, so the page is
// exercisable end to end without a live FreeBSD host or an RPC.
type stubHostPkgSource struct {
	inv   hostpkg.Inventory
	err   error
	nodes []string
}

func (s *stubHostPkgSource) HostPackageInventory(_ context.Context, nodeID string) (hostpkg.Inventory, error) {
	s.nodes = append(s.nodes, nodeID)
	if s.err != nil {
		return hostpkg.Inventory{}, s.err
	}
	return s.inv, nil
}

// withHostPkgSource installs a source for the duration of one test and
// restores the process-wide default afterwards, so tests cannot leak
// state into each other through the package-level seam.
func withHostPkgSource(t *testing.T, src hostPkgSource) {
	t.Helper()
	previous := currentHostPkgSource()
	SetHostPkgSource(src)
	t.Cleanup(func() { SetHostPkgSource(previous) })
}

// hostPkgTestInventory is a realistic partial reading: one update
// available, one current, and one whose status could not be determined.
func hostPkgTestInventory() hostpkg.Inventory {
	return hostpkg.Inventory{
		ObservedAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Base: hostpkg.BaseSystem{
			OSRelease: "15.0-RELEASE-p4", OSVersion: "15.0-RELEASE-p4", Observed: true,
			UpdateStatus: hostpkg.UpdateStatusCurrent,
			Detail:       "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date",
		},
		Catalogue: hostpkg.Catalogue{State: hostpkg.CatalogueFresh, Detail: "last refreshed 1d0h ago"},
		Ports: []hostpkg.InstalledPackage{
			{Name: "bash", Version: "5.2.26", Origin: "shells/bash", UpdateStatus: hostpkg.UpdateStatusOutdated, Candidate: "5.2.37", Detail: "pkg reports bash-5.2.26 installed, 5.2.37 available"},
			{Name: "curl", Version: "8.14.1", Origin: "www/curl", UpdateStatus: hostpkg.UpdateStatusCurrent},
			{Name: "nginx", Version: "1.26.0", Origin: "www/nginx", UpdateStatus: hostpkg.UpdateStatusUnknown, Detail: "`pkg outdated` produced output this build cannot read, so this package's update status is unobserved"},
		},
		Unknown:              []hostpkg.Unknown{{Subject: "update availability", Reason: "`pkg outdated` produced 1 line(s) this build cannot read, so no package can be called current"}},
		RawOutdatedOutput:    "something unfamiliar\n",
		RawBaseVersionOutput: "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n",
	}
}

// serveHostPkgPage drives the real route through the real mux, so the
// test covers routing, the role gate and rendering together.
func serveHostPkgPage(t *testing.T, src hostPkgSource, path string) string {
	t.Helper()
	if src != nil {
		withHostPkgSource(t, src)
	} else {
		withHostPkgSource(t, nil)
	}
	s := newTestServer(t, &fakeClient{})
	registerHostPkgRoutes(s)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestHostPkgPageRendersEveryVerdictDistinctly(t *testing.T) {
	body := serveHostPkgPage(t, &stubHostPkgSource{inv: hostPkgTestInventory()}, "/host/node-a/packages")

	for _, want := range []string{
		"FreeBSD base system",
		"15.0-RELEASE-p4",
		"Installed packages",
		"bash", "5.2.26", "5.2.37", "shells/bash",
		"curl", "nginx",
		"Package catalogue",
		"What could not be determined",
		"cannot read",
		"Reading this page requires no privilege",
		"no unattended upgrade path",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// The central rendering guarantee: a package whose status is unknown is
// never rendered as current. The badge vocabulary and the surrounding
// prose are both checked, because a green row is the failure and a
// correct-looking grey one is the fix.
func TestHostPkgPageUnknownNeverRendersAsCurrent(t *testing.T) {
	body := serveHostPkgPage(t, &stubHostPkgSource{inv: hostPkgTestInventory()}, "/host/node-a/packages")

	nginxRow := packageRowFor(t, body, "nginx")
	if !strings.Contains(nginxRow, `class="badge unknown"`) {
		t.Errorf("nginx row = %s, want an unknown badge; an unobserved package must never render as ready", nginxRow)
	}
	if !strings.Contains(nginxRow, "cannot read") {
		t.Errorf("nginx row = %s, want the reason it is unobserved shown next to it", nginxRow)
	}
	// The current package does get the ready badge, so the two are
	// genuinely distinguished rather than everything being grey.
	curlRow := packageRowFor(t, body, "curl")
	if !strings.Contains(curlRow, `class="badge ready"`) {
		t.Errorf("curl row = %s, want a ready badge", curlRow)
	}
	if strings.Contains(nginxRow, `class="badge ready"`) {
		t.Error("an unknown package rendered with the ready badge")
	}
}

func TestHostPkgPageOutdatedPackageCarriesItsCandidate(t *testing.T) {
	body := serveHostPkgPage(t, &stubHostPkgSource{inv: hostPkgTestInventory()}, "/host/node-a/packages")
	row := packageRowFor(t, body, "bash")
	if !strings.Contains(row, "5.2.37") {
		t.Errorf("bash row = %s, want the candidate version shown", row)
	}
	if !strings.Contains(row, `class="badge degraded"`) {
		t.Errorf("bash row = %s, want the degraded badge for an available update", row)
	}
	// A current package shows no candidate at all, rather than showing
	// its own version twice.
	if strings.Count(packageRowFor(t, body, "curl"), "8.14.1") != 1 {
		t.Errorf("curl row repeats its installed version as a candidate: %s", packageRowFor(t, body, "curl"))
	}
}

// With an unknown on the page, the top-line verdict is unknown - not
// "updates available", even though one update genuinely is available.
// The conservative answer outranks the positive one.
func TestHostPkgPageUnknownHeadlineOutranksAvailableUpdates(t *testing.T) {
	body := serveHostPkgPage(t, &stubHostPkgSource{inv: hostPkgTestInventory()}, "/host/node-a/packages")
	if !strings.Contains(body, `class="badge unknown">unknown</span>`) {
		t.Errorf("headline badge missing:\n%s", headlineRegion(t, body))
	}
	if !strings.Contains(body, "will not claim it is up to date") {
		t.Error("the headline explanation must say the page will not claim the host is up to date")
	}
	if !strings.Contains(body, "deliberately not reporting a clean host") {
		t.Error("an unknown headline needs an explicit warning, not a footnote")
	}
}

func TestHostPkgPageUpToDateHost(t *testing.T) {
	inv := hostPkgTestInventory()
	inv.Unknown = nil
	inv.Ports[0].UpdateStatus = hostpkg.UpdateStatusCurrent
	inv.Ports[0].Candidate = ""
	inv.Ports[0].Detail = ""
	inv.Ports[2].UpdateStatus = hostpkg.UpdateStatusCurrent
	inv.Ports[2].Detail = ""
	body := serveHostPkgPage(t, &stubHostPkgSource{inv: inv}, "/host/node-a/packages")

	if !strings.Contains(body, `class="badge ready">up_to_date</span>`) {
		t.Errorf("headline = %s, want up_to_date with the ready badge", headlineRegion(t, body))
	}
	if !strings.Contains(body, "positively read as current") {
		t.Error("the up-to-date explanation must say the claim is evidence-backed")
	}
	if strings.Contains(body, "What could not be determined") {
		t.Error("a fully readable host must not render an evidence-gap section")
	}
}

func TestHostPkgPageUpdatesAvailableHeadline(t *testing.T) {
	inv := hostPkgTestInventory()
	inv.Unknown = nil
	inv.Ports[1].UpdateStatus = hostpkg.UpdateStatusCurrent
	inv.Ports[2].UpdateStatus = hostpkg.UpdateStatusCurrent
	body := serveHostPkgPage(t, &stubHostPkgSource{inv: inv}, "/host/node-a/packages")
	if !strings.Contains(body, `class="badge degraded">updates_available</span>`) {
		t.Errorf("headline = %s, want updates_available with the degraded badge", headlineRegion(t, body))
	}
}

func TestHostPkgPageStaleCatalogueIsVisible(t *testing.T) {
	inv := hostPkgTestInventory()
	inv.Catalogue = hostpkg.Catalogue{State: hostpkg.CatalogueStale, Detail: "last refreshed 90d0h ago, limit is 14d0h"}
	body := serveHostPkgPage(t, &stubHostPkgSource{inv: inv}, "/host/node-a/packages")
	if !strings.Contains(body, `class="badge degraded">stale</span>`) {
		t.Error("a stale catalogue must be visible in its own panel")
	}
	if !strings.Contains(body, "90d0h ago") {
		t.Error("the catalogue's own age must be shown next to its verdict")
	}
	if !strings.Contains(body, "never runs <code>pkg update</code>") {
		t.Error("the page must state that it never refreshes the catalogue itself")
	}
}

// No configured source is an explicit, honest failure panel - never an
// empty table, which would read as "this host has no packages".
func TestHostPkgPageWithoutSourceSaysSo(t *testing.T) {
	body := serveHostPkgPage(t, nil, "/host/node-a/packages")
	if !strings.Contains(body, "Package state could not be read") {
		t.Errorf("missing the unavailable panel:\n%s", body)
	}
	if !strings.Contains(body, "not a claim that the host is up to date") {
		t.Error("the unavailable panel must say it is not claiming the host is current")
	}
	if strings.Contains(body, "Installed packages") {
		t.Error("an unavailable reading must not render a package table")
	}
	if strings.Contains(body, "up_to_date") {
		t.Error("an unavailable reading rendered a verdict of up to date")
	}
}

func TestHostPkgPageSourceErrorIsHonest(t *testing.T) {
	body := serveHostPkgPage(t, &stubHostPkgSource{err: errors.New("no managerd on that node")}, "/host/node-b/packages")
	if !strings.Contains(body, "could not read the package inventory for node-b") {
		t.Error("the failure must name the Comb it was asked about")
	}
	if !strings.Contains(body, "unobserved, which is not the same as up to date") {
		t.Error("a failed read must be distinguished from an up-to-date host")
	}
	if strings.Contains(body, "up_to_date") {
		t.Error("a failed read rendered a verdict of up to date")
	}
}

func TestHostPkgPagePassesTheRequestedNodeToTheSource(t *testing.T) {
	src := &stubHostPkgSource{inv: hostPkgTestInventory()}
	serveHostPkgPage(t, src, "/host/node-c/packages")
	if len(src.nodes) != 1 || src.nodes[0] != "node-c" {
		t.Errorf("source was asked for %v, want exactly [node-c]", src.nodes)
	}
}

func TestHostPkgPageIsViewerReachable(t *testing.T) {
	withHostPkgSource(t, &stubHostPkgSource{inv: hostPkgTestInventory()})
	s, err := NewServer(&fakeClient{}, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	registerHostPkgRoutes(s)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/host/node-a/packages", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET = %d, want 200: reading packages needs no privilege", rec.Code)
	}
	// No other HTTP method may reach it: a page with no write action
	// must not answer a POST.
	post := httptest.NewRecorder()
	s.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/host/node-a/packages", nil))
	if post.Code == http.StatusOK {
		t.Error("POST is served with 200; this page has no write action and must not accept one")
	}
}

func TestHostPkgSourceDefaultIsNil(t *testing.T) {
	if currentHostPkgSource() != nil {
		t.Error("the default source must be nil, so an unconfigured deployment reports unavailable rather than guessing")
	}
}

func TestHostPkgViewFromUnknownCounts(t *testing.T) {
	view := hostPkgViewFrom(hostPkgTestInventory(), "node-a")
	if view.OutdatedCount != 1 {
		t.Errorf("OutdatedCount = %d, want 1", view.OutdatedCount)
	}
	if view.UnknownCount != 1 {
		t.Errorf("UnknownCount = %d, want 1", view.UnknownCount)
	}
	if len(view.Unknowns) != 1 || view.Unknowns[0].Reason == "" {
		t.Errorf("Unknowns = %+v, want one entry with a reason", view.Unknowns)
	}
	if view.Base.Badge != "ready" {
		t.Errorf("base badge = %q, want ready", view.Base.Badge)
	}
	if view.Catalogue.Badge != "ready" {
		t.Errorf("catalogue badge = %q, want ready", view.Catalogue.Badge)
	}
}

// An unobserved base kernel is rendered as "not observed" rather than as
// an empty version field that looks like a missing value.
func TestHostPkgViewFromUnobservedBase(t *testing.T) {
	inv := hostPkgTestInventory()
	inv.Base.Observed = false
	inv.Base.OSRelease = ""
	inv.Base.OSVersion = ""
	inv.Base.UpdateStatus = hostpkg.UpdateStatusUnknown
	body := serveHostPkgPage(t, &stubHostPkgSource{inv: inv}, "/host/node-a/packages")
	if !strings.Contains(body, `class="badge unknown"`) {
		t.Error("an unobserved base must render an unknown badge")
	}
}

// packageRowFor returns the <tr> ... </tr> containing name, so a test
// can assert about one row without a brittle whole-page substring.
func packageRowFor(t *testing.T, body, name string) string {
	t.Helper()
	idx := strings.Index(body, "<code>"+name+"</code>")
	if idx < 0 {
		t.Fatalf("no row found for package %q", name)
	}
	start := strings.LastIndex(body[:idx], "<tr>")
	end := strings.Index(body[idx:], "</tr>")
	if start < 0 || end < 0 {
		t.Fatalf("could not delimit the row for %q", name)
	}
	return body[start : idx+end]
}

// headlineRegion returns the page's first headline panel, for readable
// assertion output.
func headlineRegion(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "badge ")
	if start < 0 {
		return body
	}
	end := start + 400
	if end > len(body) {
		end = len(body)
	}
	return body[start:end]
}
