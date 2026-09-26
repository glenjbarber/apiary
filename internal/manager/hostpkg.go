package manager

import (
	"context"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostpkg"
)

// HostPackages implements the read half of the host package inventory
// (internal/hostpkg) over RPC.
//
// Node-local, like HostStats: it answers about *this* node, is never
// routed through raft, and is never leader-forwarded. A caller wanting
// the whole cluster asks each node in turn, which is exactly what the
// Combs page already does for HostStats. node_id is not a request
// parameter for that reason - a package name is only meaningful to the
// one node that has it installed, so "ask this node about that other
// node" is not a question that has an honest answer.
//
// There is deliberately no apply/upgrade RPC here. internal/hostpkg has
// a guarded, single-argv Applier behind a privilege check, but which
// role may trigger a root package upgrade, and whether the confirmation
// must survive a managerd restart the way the existing pending-restart
// lease does, are decisions this change does not make. Exposing the read
// half while leaving the write half unbuilt is the reversible half.
//
// The collector runs on every call rather than being cached: `pkg
// query` and `pkg outdated` are unprivileged and cheap, and a cached
// inventory is one more thing that can go quietly stale - which, in a
// feature whose entire point is not reporting a stale answer as a
// current one, would be self-defeating.
func (s *Server) HostPackages(ctx context.Context, _ *rpcpb.HostPackagesRequest) (*rpcpb.HostPackagesResponse, error) {
	inv, err := s.hostPkgCollect(ctx)
	if err != nil {
		// The collector failed outright, so there is no reading at all.
		// That is a different condition from a reading that exists but
		// contains unknowns, and a caller must be able to tell them
		// apart: this returns an error with the node identified and no
		// inventory fields populated, so a client cannot mistake an
		// empty-but-present response for a host with nothing installed.
		return &rpcpb.HostPackagesResponse{
			NodeId: s.nodeID,
			Error:  err.Error(),
		}, nil
	}

	return hostPackagesResponse(s.nodeID, inv), nil
}

// hostPackagesResponse translates an inventory into its wire form.
//
// The whole function exists so the translation - not the shelling out -
// is what tests exercise. Every enum mapping below is total, and its
// zero value is UNKNOWN by construction: an unrecognised source value
// and an unset field both land on "we could not determine this", which
// is the only safe place for a value nobody can vouch for to land.
func hostPackagesResponse(nodeID string, inv hostpkg.Inventory) *rpcpb.HostPackagesResponse {
	resp := &rpcpb.HostPackagesResponse{
		NodeId:         nodeID,
		Base:           hostBaseSystemProto(inv.Base),
		Catalogue:      hostCatalogueProto(inv.Catalogue),
		Ports:          make([]*rpcpb.HostInstalledPackage, 0, len(inv.Ports)),
		Unknown:        make([]*rpcpb.HostPackageUnknown, 0, len(inv.Unknown)),
		Headline:       string(inv.Headline()),
		ObservedAtUnix: unixOrZero(inv.ObservedAt),
	}

	for _, p := range inv.Ports {
		resp.Ports = append(resp.Ports, &rpcpb.HostInstalledPackage{
			Name:             p.Name,
			Version:          p.Version,
			Origin:           p.Origin,
			UpdateStatus:     hostUpdateStatusProto(p.UpdateStatus),
			CandidateVersion: p.Candidate,
			Detail:           p.Detail,
		})
	}

	// The per-item Unknown list is carried verbatim as well as being
	// folded into each item's verdict, because the two answer different
	// questions: this list is the collective "here is everything we
	// could not determine", while each item's own verdict is "and here
	// is what that particular thing's answer is". A client can render
	// either without deriving the other.
	for _, u := range inv.Unknown {
		resp.Unknown = append(resp.Unknown, &rpcpb.HostPackageUnknown{
			Subject: u.Subject,
			Reason:  u.Reason,
		})
	}

	return resp
}

func hostBaseSystemProto(b hostpkg.BaseSystem) *rpcpb.HostBaseSystem {
	return &rpcpb.HostBaseSystem{
		// OSRelease is uname -r (the running kernel) and OSVersion is
		// uname -v, which is the opposite of what the two names suggest
		// - see hostpkg.BaseSystem's own field docs. The wire names
		// carry that meaning explicitly so the swap cannot be repeated.
		KernelRelease: b.OSRelease,
		Version:       b.OSVersion,
		Pkgbase:       b.PkgBasePackage,
		UpdateStatus:  hostUpdateStatusProto(b.UpdateStatus),
		Observed:      b.Observed,
		Detail:        b.Detail,
	}
}

func hostCatalogueProto(c hostpkg.Catalogue) *rpcpb.HostPackageCatalogue {
	state := rpcpb.HostPackageCatalogueState_HOST_PACKAGE_CATALOGUE_STATE_UNSPECIFIED
	switch c.State {
	case hostpkg.CatalogueFresh:
		state = rpcpb.HostPackageCatalogueState_HOST_PACKAGE_CATALOGUE_STATE_FRESH
	case hostpkg.CatalogueStale:
		state = rpcpb.HostPackageCatalogueState_HOST_PACKAGE_CATALOGUE_STATE_STALE
	case hostpkg.CatalogueUnknown:
		// Already the zero value. Left explicit so the mapping is
		// visibly total and a reader can see the unknown case was
		// considered rather than missed.
	}

	return &rpcpb.HostPackageCatalogue{
		Path:           c.Path,
		State:          state,
		ModifiedAtUnix: unixOrZero(c.ModifiedAt),
		AgeSeconds:     int64(c.Age / time.Second),
		Detail:         c.Detail,
	}
}

// hostUpdateStatusProto maps hostpkg's UpdateStatus onto the wire enum.
//
// The switch is total and the default arm is UNKNOWN, not CURRENT. That
// is the load-bearing decision in this function: a status value this
// build does not recognise is evidence of nothing, and reporting it as
// "current" would tell an operator their host is fine on the strength of
// a value nobody can interpret. Unknown is the only defensible landing
// spot.
func hostUpdateStatusProto(s hostpkg.UpdateStatus) rpcpb.HostPackageUpdateStatus {
	switch s {
	case hostpkg.UpdateStatusCurrent:
		return rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_CURRENT
	case hostpkg.UpdateStatusOutdated:
		return rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_OUTDATED
	default:
		return rpcpb.HostPackageUpdateStatus_HOST_PACKAGE_UPDATE_STATUS_UNSPECIFIED
	}
}

// unixOrZero converts a timestamp to Unix seconds, mapping the zero time
// to 0. A zero Unix time means "never observed" everywhere it appears on
// this message; that is why a time.Time zero value must not be allowed
// through as a large negative number, which a client would read as a
// real moment far in the past.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
