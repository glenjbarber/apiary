package frontend

// Transport for the host package page (internal/hostpkg, see
// hostpkg_page.go for the page's own contract and the honest-unread
// rules it follows).
//
// This file exists to bridge the new ManagerService.HostPackages RPC to
// the page's existing hostPkgSource interface. It deliberately converts
// the wire response BACK into a hostpkg.Inventory rather than teaching
// the page about protobuf:
//
//   - hostpkg_page.go's translation and rendering, and the tests over it,
//     stay exactly as they were written and keep being the single place
//     that decides what an operator is shown. Adding a second, parallel
//     proto-shaped rendering path is how two views of the same evidence
//     start disagreeing - and here the disagreement would be about
//     whether a host is up to date.
//   - The unknown/fresh/outdated vocabulary stays defined once, in
//     hostpkg, instead of being re-spelled as enum constants on this
//     side of the wire.
//
// The cost is one lossy round trip, which is acceptable because the
// mapping is total in the safe direction: any value this file does not
// recognise becomes UpdateStatusUnknown or CatalogueUnknown, never
// "current" and never "fresh".

import (
	"context"
	"fmt"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostpkg"
)

// EnableHostPkgSource wires the host package page to this server's own
// managerd client and peer dialer, and makes the page reachable.
//
// It is a separate call rather than something NewServer does
// unconditionally, because the page is useless without a source and a
// source that silently appears means a page that silently works in
// development and 404s in production. Making it explicit means the
// absence of a page and the absence of a transport are the same
// decision, made in one visible place.
func (s *Server) EnableHostPkgSource() {
	// Asserted rather than added to peerHostStatsClient: widening that
	// interface would force every existing peer fake in this package to
	// grow a method they have no use for. A peer dialer that cannot
	// reach HostPackages gets an honest "peer reads unavailable" below
	// rather than being handed a nil that would panic on first use.
	var pkgPeers peerHostPkgClient
	if p, ok := s.peers.(peerHostPkgClient); ok {
		pkgPeers = p
	}

	SetHostPkgSource(managerdHostPkgSource{
		client: s.client,
		peers:  pkgPeers,
		peerAddr: func(nodeID string) string {
			return s.peerAddr(nodeID)
		},
		localNodeIDGetter: func(ctx context.Context) (string, error) {
			// Asked per call rather than cached: this frontend's own
			// node ID can change if managerd is reconfigured, and a
			// cached wrong value would send every lookup to the wrong
			// node. A failure here is not fatal - the source falls back
			// to asking locally, and the NodeId check on the response
			// catches it.
			resp, err := s.client.Status(ctx, &rpcpb.StatusRequest{})
			if err != nil {
				return "", err
			}
			return resp.GetManagerNodeId(), nil
		},
	})
}

// peerHostPkgClient is the subset of *manager.PeerReporter needed to
// reach another node's managerd for its package inventory. Defined
// locally for the same reason peerHostStatsClient is: the RPC is
// node-local, so reaching a node this frontend is not colocated with
// means dialing that node directly.
type peerHostPkgClient interface {
	HostPackages(ctx context.Context, addr string) (*rpcpb.HostPackagesResponse, error)
}

// managerdHostPkgSource reads a Comb's package inventory through
// managerd: directly via s.client for this frontend's own colocated
// node, otherwise by dialing that node through s.peers - the same
// local/peer split fetchHostStats already makes.
type managerdHostPkgSource struct {
	client rpcpb.ManagerServiceClient
	peers  peerHostPkgClient

	peerAddr          func(nodeID string) string
	localNodeIDGetter func(ctx context.Context) (string, error)
}

// HostPackageInventory implements hostPkgSource.
//
// Two failures are kept distinct on purpose, because collapsing them is
// exactly the bug this feature exists to prevent:
//
//  1. The node could not be reached or did not answer. There is no
//     reading at all.
//  2. A reading came back, and it is unobserved or partly undetermined.
//
// Both are errors to this interface - a caller gets either an Inventory
// or an error, never a zero Inventory that reads as "no packages, all
// fine" - and the error text says which of the two happened so the page
// can say so too.
func (s managerdHostPkgSource) HostPackageInventory(ctx context.Context, nodeID string) (hostpkg.Inventory, error) {
	resp, err := s.fetch(ctx, nodeID)
	if err != nil {
		return hostpkg.Inventory{}, fmt.Errorf("could not reach %s's managerd for its package inventory: %w "+
			"- this Comb's packages are unobserved, which is not the same as up to date", nodeID, err)
	}

	// The RPC answers for whoever receives it, so node_id is not a
	// request parameter and the only way to know what was actually read
	// is to check what came back. A mismatch means the response is
	// about some other host, and presenting it under this node's name
	// would be a confident wrong answer rather than an obvious
	// failure - so it is refused instead.
	if got := resp.GetNodeId(); got != nodeID {
		return hostpkg.Inventory{}, fmt.Errorf("asked %s for its package inventory but the response is about %q "+
			"- refusing to present another Comb's packages as this one's", nodeID, got)
	}

	if errText := resp.GetError(); errText != "" {
		return hostpkg.Inventory{}, fmt.Errorf("%s's managerd could not read its package inventory: %s "+
			"- this Comb's packages are unobserved, which is not the same as up to date", nodeID, errText)
	}

	return hostPkgInventoryFromProto(resp), nil
}

func (s managerdHostPkgSource) fetch(ctx context.Context, nodeID string) (*rpcpb.HostPackagesResponse, error) {
	if s.peers == nil {
		return s.client.HostPackages(ctx, &rpcpb.HostPackagesRequest{})
	}
	localNodeID, err := s.localNodeIDGetter(ctx)
	if err != nil || nodeID == localNodeID {
		// Either this frontend cannot determine which node it is
		// colocated with, or the requested node IS that node. Both
		// mean "ask our own managerd", which is what fetchHostStats
		// does in the same situation. Asking locally is the safe
		// default here because the NodeId check above catches the case
		// where it was wrong.
		return s.client.HostPackages(ctx, &rpcpb.HostPackagesRequest{})
	}
	return s.peers.HostPackages(ctx, s.peerAddr(nodeID))
} // inventory the page renders.
// Every enum mapping here is total and its default arm is the UNKNOWN
// value, for the same reason the managerd-side mapping is: a value this
// build cannot interpret is evidence of nothing, and the only honest
// thing to do with it is record it as undetermined.
func hostPkgInventoryFromProto(resp *rpcpb.HostPackagesResponse) hostpkg.Inventory {
	inv := hostpkg.Inventory{
		ObservedAt: time.Unix(resp.GetObservedAtUnix(), 0),
		Base:       hostPkgBaseFromProto(resp.GetBase()),
		Catalogue:  hostPkgCatalogueFromProto(resp.GetCatalogue()),
		Ports:      make([]hostpkg.InstalledPackage, 0, len(resp.GetPorts())),
		Unknown:    make([]hostpkg.Unknown, 0, len(resp.GetUnknown())),
	}

	// A zero observed_at means the reading's own time could not be
	// established. time.Unix(0,0) is 1970, which the page would render
	// as a real moment, so it is normalised back to the zero Time -
	// which is how hostpkg itself spells "never observed".
	if resp.GetObservedAtUnix() == 0 {
		inv.ObservedAt = time.Time{}
	}

	for _, p := range resp.GetPorts() {
		inv.Ports = append(inv.Ports, hostpkg.InstalledPackage{
			Name:         p.GetName(),
			Version:      p.GetVersion(),
			Origin:       p.GetOrigin(),
			UpdateStatus: hostPkgUpdateStatusFromProto(p.GetUpdateStatus()),
			Candidate:    p.GetCandidateVersion(),
			Detail:       p.GetDetail(),
		})
	}

	for _, u := range resp.GetUnknown() {
		inv.Unknown = append(inv.Unknown, hostpkg.Unknown{
			Subject: u.GetSubject(),
			Reason:  u.GetReason(),
		})
	}

	return inv
}

func hostPkgBaseFromProto(b *rpcpb.HostBaseSystem) hostpkg.BaseSystem {
	return hostpkg.BaseSystem{
		OSRelease:      b.GetKernelRelease(),
		OSVersion:      b.GetVersion(),
		PkgBasePackage: b.GetPkgbase(),
		UpdateStatus:   hostPkgUpdateStatusFromProto(b.GetUpdateStatus()),
		Detail:         b.GetDetail(),
		Observed:       b.GetObserved(),
	}
}

func hostPkgCatalogueFromProto(c *rpcpb.HostPackageCatalogue) hostpkg.Catalogue {
	cat := hostpkg.Catalogue{
		Path:   c.GetPath(),
		Detail: c.GetDetail(),
		Age:    time.Duration(c.GetAgeSeconds()) * time.Second,
	}

	// Both timestamps are 0-means-unknown on the wire. Normalising both
	// back to the zero Time keeps hostpkg's own "did we read it?"
	// checks meaningful instead of handing them 1970.
	if c.GetModifiedAtUnix() != 0 {
		cat.ModifiedAt = time.Unix(c.GetModifiedAtUnix(), 0)
	}
	if c.GetAgeSeconds() == 0 {
		cat.Age = 0
	}

	switch c.GetState() {
	case rpcpb.HostPackageCatalogueState_HOST_PACKAGE_CATALOGUE_STATE_FRESH:
		cat.State = hostpkg.CatalogueFresh
	case rpcpb.HostPackageCatalogueState_HOST_PACKAGE_CATALOGUE_STATE_STALE:
		cat.State = hostpkg.CatalogueStale
	default:
		// Includes UNSPECIFIED and any value a newer peer might send:
		// unknown, which downgrades every verdict below it.
		cat.State = hostpkg.CatalogueUnknown
	}

	return cat
}

func hostPkgUpdateStatusFromProto(s rpcpb.HostPackageUpdateStatus) hostpkg.UpdateStatus {
	switch s {
	case rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_CURRENT:
		return hostpkg.UpdateStatusCurrent
	case rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_OUTDATED:
		return hostpkg.UpdateStatusOutdated
	default:
		// UNSPECIFIED, or a value a newer peer sent. Unknown - never
		// current. A host whose status this build cannot read must not
		// be reported as up to date.
		return hostpkg.UpdateStatusUnknown
	}
}
