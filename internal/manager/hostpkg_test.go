package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostpkg"
)

// observed is a fixed instant so every assertion below is deterministic
// and so a test can never pass because the wall clock happened to agree.
var observed = time.Date(2026, 9, 26, 14, 5, 0, 0, time.UTC)

func TestHostPackagesResponse_CurrentVerdictsTranslate(t *testing.T) {
	inv := hostpkg.Inventory{
		ObservedAt: observed,
		Base: hostpkg.BaseSystem{
			OSRelease:      "16.0-CURRENT",
			OSVersion:      "FreeBSD 16.0-CURRENT #1: Fri Sep  5 00:00:00 UTC 2026 root@releng/amd64",
			PkgBasePackage: true,
			UpdateStatus:   hostpkg.UpdateStatusCurrent,
			Observed:       true,
			Detail:         "FreeBSD is up to date",
		},
		Catalogue: hostpkg.Catalogue{
			Path:       hostpkg.DefaultCataloguePath,
			State:      hostpkg.CatalogueFresh,
			ModifiedAt: observed.Add(-24 * time.Hour),
			Age:        24 * time.Hour,
		},
		Ports: []hostpkg.InstalledPackage{{
			Name:         "nginx",
			Version:      "1.26.1",
			Origin:       "www/nginx",
			UpdateStatus: hostpkg.UpdateStatusCurrent,
		}},
	}

	got := hostPackagesResponse("brood", inv)

	if got.GetNodeId() != "brood" {
		t.Errorf("node_id = %q, want brood", got.GetNodeId())
	}
	if got.GetHeadline() != string(hostpkg.HeadlineUpToDate) {
		t.Errorf("headline = %q, want up_to_date", got.GetHeadline())
	}
	if got.GetObservedAtUnix() != observed.Unix() {
		t.Errorf("observed_at = %d, want %d", got.GetObservedAtUnix(), observed.Unix())
	}
	if k := got.GetBase().GetKernelRelease(); k != "16.0-CURRENT" {
		t.Errorf("kernel_release = %q, want 16.0-CURRENT", k)
	}
	if !got.GetBase().GetPkgbase() {
		t.Error("pkgbase = false, want true on a pkgbase host")
	}
	if got.GetCatalogue().GetState() != rpcpb.HostPackageCatalogueState_HOST_PACKAGE_CATALOGUE_STATE_FRESH {
		t.Errorf("catalogue state = %v, want FRESH", got.GetCatalogue().GetState())
	}
	if len(got.GetPorts()) != 1 || got.GetPorts()[0].GetName() != "nginx" {
		t.Fatalf("ports = %v, want one nginx", got.GetPorts())
	}
	if got.GetPorts()[0].GetUpdateStatus() != rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_CURRENT {
		t.Errorf("nginx status = %v, want CURRENT", got.GetPorts()[0].GetUpdateStatus())
	}
}

// An outdated package must carry the candidate version, and an outdated
// BASE must be distinguishable from an outdated package, because the two
// need different operator actions (a reboot, versus an install).
func TestHostPackagesResponse_OutdatedCarriesCandidate(t *testing.T) {
	inv := hostpkg.Inventory{
		ObservedAt: observed,
		Base: hostpkg.BaseSystem{
			OSRelease:    "15.1-RELEASE",
			UpdateStatus: hostpkg.UpdateStatusOutdated,
			Observed:     true,
		},
		Ports: []hostpkg.InstalledPackage{{
			Name:         "nginx",
			Version:      "1.26.1",
			UpdateStatus: hostpkg.UpdateStatusOutdated,
			Candidate:    "1.27.0",
		}},
	}

	got := hostPackagesResponse("brood", inv)

	if got.GetHeadline() != string(hostpkg.HeadlineUpdatesAvailable) {
		t.Errorf("headline = %q, want updates_available", got.GetHeadline())
	}
	if got.GetBase().GetUpdateStatus() != rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_OUTDATED {
		t.Error("base status = not OUTDATED; a reboot signal was lost")
	}
	p := got.GetPorts()[0]
	if p.GetUpdateStatus() != rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_OUTDATED {
		t.Error("package status = not OUTDATED")
	}
	if p.GetCandidateVersion() != "1.27.0" {
		t.Errorf("candidate = %q, want 1.27.0", p.GetCandidateVersion())
	}
}

// The load-bearing test: an UNKNOWN verdict must never reach the wire as
// CURRENT. hostpkg already refuses to produce a current verdict without
// evidence, and this guards the translation that could undo that.
func TestHostPackagesResponse_UnknownNeverBecomesCurrent(t *testing.T) {
	inv := hostpkg.Inventory{
		ObservedAt: observed,
		Base: hostpkg.BaseSystem{
			OSRelease:    "15.1-RELEASE",
			UpdateStatus: hostpkg.UpdateStatusUnknown,
			Observed:     true,
			Detail:       "local pkg catalogue is 30 days old; run pkg update",
		},
		Catalogue: hostpkg.Catalogue{State: hostpkg.CatalogueStale},
		Ports: []hostpkg.InstalledPackage{{
			Name:         "nginx",
			UpdateStatus: hostpkg.UpdateStatusUnknown,
		}},
		Unknown: []hostpkg.Unknown{{
			Subject: "nginx",
			Reason:  "unparseable pkg outdated line",
		}},
	}

	got := hostPackagesResponse("brood", inv)

	if got.GetBase().GetUpdateStatus() != rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_UNSPECIFIED {
		t.Errorf("base status = %v, want UNSPECIFIED (unknown)", got.GetBase().GetUpdateStatus())
	}
	if got.GetPorts()[0].GetUpdateStatus() != rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_UNSPECIFIED {
		t.Errorf("package status = %v, want UNSPECIFIED (unknown)", got.GetPorts()[0].GetUpdateStatus())
	}
	// Any unknown at all must downgrade the headline, and unknown
	// outranks updates_available.
	if got.GetHeadline() != string(hostpkg.HeadlineUnknown) {
		t.Errorf("headline = %q, want unknown", got.GetHeadline())
	}
	if got.GetCatalogue().GetState() != rpcpb.HostPackageCatalogueState_HOST_PACKAGE_CATALOGUE_STATE_STALE {
		t.Errorf("catalogue = %v, want STALE", got.GetCatalogue().GetState())
	}
	if len(got.GetUnknown()) != 1 || got.GetUnknown()[0].GetSubject() != "nginx" {
		t.Errorf("unknown list = %v, want the nginx entry preserved", got.GetUnknown())
	}
}

// A status value this build has never seen must land on unknown. This is
// the case the enum's zero value was chosen for: a newer managerd can
// send a status this frontend has no name for, and "no name for it" is
// not evidence that the package is current.
func TestHostUpdateStatusProto_UnrecognisedValueIsUnknown(t *testing.T) {
	for _, s := range []hostpkg.UpdateStatus{
		"",
		"something-a-newer-build-sends",
		"CURRENT", // wrong case: still not a value we recognise
	} {
		got := hostUpdateStatusProto(s)
		if got != rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_UNSPECIFIED {
			t.Errorf("hostUpdateStatusProto(%q) = %v, want UNSPECIFIED", s, got)
		}
	}
}

// A zero Time must reach the wire as 0, not as a large negative Unix
// value. A client reading 0 knows the timestamp is unknown; a client
// reading -62135596800 would render it as a moment far in the past and
// conclude the observation is ancient.
func TestUnixOrZero_ZeroTimeIsZero(t *testing.T) {
	if got := unixOrZero(time.Time{}); got != 0 {
		t.Errorf("unixOrZero(zero) = %d, want 0", got)
	}
	if got := unixOrZero(observed); got != observed.Unix() {
		t.Errorf("unixOrZero(observed) = %d, want %d", got, observed.Unix())
	}
}

func TestHostCatalogueProto_CatalogueUnknownMapsToUnspecified(t *testing.T) {
	got := hostCatalogueProto(hostpkg.Catalogue{
		State:  hostpkg.CatalogueUnknown,
		Detail: "local.sqlite not found",
	})
	if got.GetState() != rpcpb.HostPackageCatalogueState_HOST_PACKAGE_CATALOGUE_STATE_UNSPECIFIED {
		t.Errorf("state = %v, want UNSPECIFIED (unknown)", got.GetState())
	}
	if got.GetModifiedAtUnix() != 0 {
		t.Errorf("modified_at = %d, want 0 for an unreadable catalogue", got.GetModifiedAtUnix())
	}
}

// A collector that fails outright must produce an error-carrying
// response with the node identified and NO inventory fields - so a
// client cannot mistake "nothing was read" for "this host has nothing
// installed and is therefore fine".
func TestServer_HostPackages_CollectorFailureIsNotAnEmptyInventory(t *testing.T) {
	s := &Server{nodeID: "brood"}
	s.hostPkgCollect = func(context.Context) (hostpkg.Inventory, error) {
		return hostpkg.Inventory{}, errors.New("pkg: command not found")
	}

	resp, err := s.HostPackages(context.Background(), &rpcpb.HostPackagesRequest{})
	if err != nil {
		t.Fatalf("HostPackages returned a transport error %v; the failure belongs in the response so a client can render it", err)
	}
	if resp.GetError() == "" {
		t.Fatal("error = empty; a failed collection must be reported")
	}
	if resp.GetNodeId() != "brood" {
		t.Errorf("node_id = %q, want brood even on failure", resp.GetNodeId())
	}
	if len(resp.GetPorts()) != 0 {
		t.Errorf("ports = %v, want none on a failed collection", resp.GetPorts())
	}
	// Headline is empty, not "up_to_date" - an empty response must never
	// carry a reassuring verdict.
	if resp.GetHeadline() != "" {
		t.Errorf("headline = %q, want empty on a failed collection", resp.GetHeadline())
	}
}
