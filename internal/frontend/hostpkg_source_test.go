package frontend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostpkg"
)

var hostPkgObserved = time.Date(2026, 9, 26, 14, 5, 0, 0, time.UTC)

// stubHostPkgClient answers HostPackages from a canned response, and
// records which address was dialed so a test can assert the local/peer
// split actually happened.
type stubHostPkgClient struct {
	rpcpb.ManagerServiceClient
	resp *rpcpb.HostPackagesResponse
	err  error

	dialed string // set only by the peer stub
}

func (c *stubHostPkgClient) HostPackages(context.Context, *rpcpb.HostPackagesRequest, ...grpc.CallOption) (*rpcpb.HostPackagesResponse, error) {
	return c.resp, c.err
}

type stubPeerHostPkgClient struct {
	resp *rpcpb.HostPackagesResponse
	err  error
	addr string
}

func (p *stubPeerHostPkgClient) HostPackages(_ context.Context, addr string) (*rpcpb.HostPackagesResponse, error) {
	p.addr = addr
	return p.resp, p.err
}

func hostPkgTestSource(client *stubHostPkgClient, peers peerHostPkgClient, localNodeID string) managerdHostPkgSource {
	return managerdHostPkgSource{
		client: client,
		peers:  peers,
		peerAddr: func(nodeID string) string {
			return nodeID + ".test:9443"
		},
		localNodeIDGetter: func(context.Context) (string, error) {
			if localNodeID == "" {
				return "", errors.New("Status unavailable")
			}
			return localNodeID, nil
		},
	}
}

func goodResponse(nodeID string) *rpcpb.HostPackagesResponse {
	return &rpcpb.HostPackagesResponse{
		NodeId:         nodeID,
		Headline:       "up_to_date",
		ObservedAtUnix: hostPkgObserved.Unix(),
		Base: &rpcpb.HostBaseSystem{
			KernelRelease: "16.0-CURRENT",
			Pkgbase:       true,
			UpdateStatus:  rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_CURRENT,
			Observed:      true,
		},
		Catalogue: &rpcpb.HostPackageCatalogue{
			Path:           hostpkg.DefaultCataloguePath,
			State:          rpcpb.HostPackageCatalogueState_HOST_PACKAGE_CATALOGUE_STATE_FRESH,
			ModifiedAtUnix: hostPkgObserved.Add(-time.Hour).Unix(),
			AgeSeconds:     3600,
		},
		Ports: []*rpcpb.HostInstalledPackage{{
			Name:         "nginx",
			Version:      "1.26.1",
			Origin:       "www/nginx",
			UpdateStatus: rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_CURRENT,
		}},
	}
}

func TestManagerdHostPkgSource_ReadsLocalNode(t *testing.T) {
	src := hostPkgTestSource(&stubHostPkgClient{resp: goodResponse("brood")}, nil, "brood")

	inv, err := src.HostPackageInventory(context.Background(), "brood")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(inv.Ports) != 1 || inv.Ports[0].Name != "nginx" {
		t.Fatalf("ports = %v, want nginx", inv.Ports)
	}
	if inv.Base.OSRelease != "16.0-CURRENT" {
		t.Errorf("kernel release = %q, want 16.0-CURRENT", inv.Base.OSRelease)
	}
	if inv.Catalogue.State != hostpkg.CatalogueFresh {
		t.Errorf("catalogue = %q, want fresh", inv.Catalogue.State)
	}
	if inv.Headline() != hostpkg.HeadlineUpToDate {
		t.Errorf("headline = %q, want up_to_date", inv.Headline())
	}
}

// A remote node must be reached by dialing that node, not by asking the
// local managerd - and the dialed address must be the peer's, derived
// from the node ID.
func TestManagerdHostPkgSource_DialsPeerForRemoteNode(t *testing.T) {
	peer := &stubPeerHostPkgClient{resp: goodResponse("drone")}
	src := hostPkgTestSource(&stubHostPkgClient{resp: goodResponse("brood")}, peer, "brood")

	inv, err := src.HostPackageInventory(context.Background(), "drone")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peer.addr != "drone.test:9443" {
		t.Errorf("dialed %q, want drone.test:9443", peer.addr)
	}
	if inv.Headline() != hostpkg.HeadlineUpToDate {
		t.Errorf("headline = %q, want up_to_date", inv.Headline())
	}
}

// The RPC answers for whoever receives it, so a response about a
// different node must be refused rather than presented under the
// requested node's name. This is the case that would otherwise be a
// confident wrong answer rather than an obvious failure.
func TestManagerdHostPkgSource_RefusesMismatchedNodeID(t *testing.T) {
	src := hostPkgTestSource(&stubHostPkgClient{resp: goodResponse("drone")}, nil, "brood")

	_, err := src.HostPackageInventory(context.Background(), "brood")
	if err == nil {
		t.Fatal("error = nil; a response about another Comb must be refused")
	}
	if want := "drone"; !contains(err.Error(), want) {
		t.Errorf("error %q does not name the node the response was actually about (%s)", err, want)
	}
}

// A response carrying Error means nothing was read. It must not become a
// zero Inventory, which would render as "no packages, all current".
func TestManagerdHostPkgSource_ResponseErrorIsNotAnEmptyInventory(t *testing.T) {
	src := hostPkgTestSource(&stubHostPkgClient{resp: &rpcpb.HostPackagesResponse{
		NodeId: "brood",
		Error:  "pkg: command not found",
	}}, nil, "brood")

	_, err := src.HostPackageInventory(context.Background(), "brood")
	if err == nil {
		t.Fatal("error = nil; a response-level failure must surface as an error")
	}
	if !contains(err.Error(), "unobserved") {
		t.Errorf("error %q should say the packages are unobserved, not merely that it failed", err)
	}
}

// An unreachable node is an error, never an empty inventory.
func TestManagerdHostPkgSource_UnreachableNodeIsAnError(t *testing.T) {
	src := hostPkgTestSource(&stubHostPkgClient{
		err: status.Error(codes.Unavailable, "connection refused"),
	}, nil, "brood")

	_, err := src.HostPackageInventory(context.Background(), "brood")
	if err == nil {
		t.Fatal("error = nil; an unreachable Comb must not read as an empty inventory")
	}
}

// An unrecognised status value from a newer managerd must become
// unknown. If it became "current" the frontend would show a green
// up-to-date badge for a host whose state it cannot read.
func TestManagerdHostPkgSource_UnrecognisedEnumIsUnknown(t *testing.T) {
	resp := goodResponse("brood")
	// A value this build has no name for.
	resp.Ports[0].UpdateStatus = rpcpb.HostPackageUpdateStatus(99)
	resp.Headline = ""
	resp.Unknown = []*rpcpb.HostPackageUnknown{{Subject: "nginx", Reason: "unrecognised status"}}

	src := hostPkgTestSource(&stubHostPkgClient{resp: resp}, nil, "brood")
	inv, err := src.HostPackageInventory(context.Background(), "brood")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.Ports[0].UpdateStatus != hostpkg.UpdateStatusUnknown {
		t.Errorf("status = %q, want unknown", inv.Ports[0].UpdateStatus)
	}
	if inv.Headline() != hostpkg.HeadlineUnknown {
		t.Errorf("headline = %q, want unknown", inv.Headline())
	}
}

// An unrecognised catalogue state must be unknown too, since the
// catalogue gates every verdict below it.
func TestManagerdHostPkgSource_UnrecognisedCatalogueStateIsUnknown(t *testing.T) {
	resp := goodResponse("brood")
	resp.Catalogue.State = rpcpb.HostPackageCatalogueState(99)

	src := hostPkgTestSource(&stubHostPkgClient{resp: resp}, nil, "brood")
	inv, err := src.HostPackageInventory(context.Background(), "brood")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.Catalogue.State != hostpkg.CatalogueUnknown {
		t.Errorf("catalogue = %q, want unknown", inv.Catalogue.State)
	}
}

// A zero observed_at on the wire means "unknown". time.Unix(0,0) is
// 1970, which the page would render as a real, very old moment.
func TestManagerdHostPkgSource_ZeroObservedAtIsNot1970(t *testing.T) {
	resp := goodResponse("brood")
	resp.ObservedAtUnix = 0

	src := hostPkgTestSource(&stubHostPkgClient{resp: resp}, nil, "brood")
	inv, err := src.HostPackageInventory(context.Background(), "brood")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !inv.ObservedAt.IsZero() {
		t.Errorf("observed_at = %v, want the zero Time", inv.ObservedAt)
	}
}

// When the frontend cannot determine which node it is colocated with, it
// asks locally. The NodeId check then catches it if that was wrong,
// rather than the source guessing which dialer to use.
func TestManagerdHostPkgSource_UnknownLocalNodeAsksLocally(t *testing.T) {
	peer := &stubPeerHostPkgClient{resp: goodResponse("drone")}
	src2 := hostPkgTestSource(&stubHostPkgClient{resp: goodResponse("brood")}, peer, "")
	if _, err := src2.HostPackageInventory(context.Background(), "drone"); err == nil {
		t.Fatal("error = nil; asking locally for a remote node must fail the NodeId check, not silently pass")
	}
	if peer.addr != "" {
		t.Errorf("peer was dialed (%q) when the local node is unknown; it should have asked locally", peer.addr)
	}
}

// contains is a tiny helper so these tests can assert on error text
// without pulling in strings for one call each.

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestHostPkgPageRendersALiveWireResponse is the end-to-end check: a
// response straight off the wire, with no hostpkg types involved at all,
// rendered through the real route. Every test above pins one conversion
// step; this proves the assembled path still shows the real numbers and
// does not silently render an empty or reassuring page.
func TestHostPkgPageRendersALiveWireResponse(t *testing.T) {
	withHostPkgSource(t, managerdHostPkgSource{
		client: &stubHostPkgClient{resp: goodResponse("brood")},
		localNodeIDGetter: func(context.Context) (string, error) {
			return "brood", nil
		},
	})

	s := newTestServer(t, &fakeClient{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/host/brood/packages", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"16.0-CURRENT", "nginx", "1.26.1", "www/nginx"} {
		if !contains(body, want) {
			t.Errorf("rendered page does not contain %q; the wire response did not reach the template", want)
		}
	}
	// A live, fully-known reading legitimately gets the up-to-date
	// badge. This is the ONE case where green is correct, and it is only
	// correct because the response carried no unknowns at all.
	if !contains(body, "up to date") && !contains(body, "up-to-date") && !contains(body, "Up to date") {
		t.Errorf("a response with zero unknowns should render an up-to-date verdict; body:\n%s", body)
	}
}

// A wire response carrying unknowns must never render as up to date,
// however the template phrases it.
func TestHostPkgPageDoesNotRenderUnknownAsUpToDate(t *testing.T) {
	resp := goodResponse("brood")
	resp.Headline = "unknown"
	resp.Unknown = []*rpcpb.HostPackageUnknown{{
		Subject: "nginx",
		Reason:  "local pkg catalogue is 30 days old; run pkg update",
	}}
	resp.Ports[0].UpdateStatus = rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_UNSPECIFIED

	withHostPkgSource(t, managerdHostPkgSource{
		client: &stubHostPkgClient{resp: resp},
		localNodeIDGetter: func(context.Context) (string, error) {
			return "brood", nil
		},
	})

	s := newTestServer(t, &fakeClient{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/host/brood/packages", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if contains(rec.Body.String(), "Everything is up to date") {
		t.Errorf("page claims everything is up to date despite a carried unknown:\n%s", rec.Body.String())
	}
	// The reason must be shown, not just the verdict: an unexplained
	// unknown is indistinguishable from a bug.
	if !contains(rec.Body.String(), "pkg update") {
		t.Errorf("page does not show why the reading is undetermined:\n%s", rec.Body.String())
	}
}

// TestHostPackagesLinkEscapesNodeID pins the escaping. The link is built
// into pageHeader's extra slot, which is passed through as raw HTML, so
// an unescaped node ID would be an HTML-injection vector reachable by
// anything that can influence a node ID - including a join request.
func TestHostPackagesLinkEscapesNodeID(t *testing.T) {
	for _, nodeID := range []string{
		`brood`,
		`"><script>alert(1)</script>`,
		`a"b`,
		`a<b`,
	} {
		got := hostPackagesLink(nodeID)

		// The href attribute must not be terminated early, and no raw
		// angle bracket from the node ID may survive.
		if strings.Contains(got, `"><`) {
			t.Errorf("hostPackagesLink(%q) = %q; the attribute was broken out of", nodeID, got)
		}
		if strings.Contains(got, "<script") {
			t.Errorf("hostPackagesLink(%q) = %q; raw markup survived escaping", nodeID, got)
		}
		// The href must still be present and well formed.
		const want = `href="/host/`
		if !strings.Contains(got, want) {
			t.Errorf("hostPackagesLink(%q) = %q; want it to contain %q", nodeID, got, want)
		}
	}

	// A normal ID is unchanged - escaping must not mangle the common case.
	if got := hostPackagesLink("brood"); !strings.Contains(got, "/host/brood/packages") {
		t.Errorf("hostPackagesLink(\"brood\") = %q; a plain node ID must pass through intact", got)
	}
}
