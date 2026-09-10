// VM ZFS snapshot/restore (ADR-0090) - a VM's own dataset (which holds
// its disk.img) can be checkpointed and rolled back, so an operator can
// undo a bad in-guest change without rebuilding the VM from scratch.
// Real local ZFS state, never routed through raft - every action here
// mirrors serial_log.go's own owner-forwarding pattern exactly: use
// this frontend's own colocated managerd directly when it happens to
// own the VM, otherwise dial the owning Comb's managerd via s.peers.
package frontend

import (
	"context"
	"net/http"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// ownerManagerdAddrForVM resolves where to send a VM-owner-scoped
// request: empty string means "use s.client directly, this frontend's
// own colocated managerd already owns it (or ownership couldn't be
// determined, or peers isn't configured - fall back to the local
// managerd's own clear owner-mismatch error rather than fail outright)".
// A non-empty return is the address to dial via s.peers instead.
// Mirrors serialLogForVM's own resolution exactly.
func (s *Server) ownerManagerdAddrForVM(ctx context.Context, id string) string {
	vmResp, err := s.client.GetVM(ctx, &rpcpb.GetVMRequest{Id: id})
	if err != nil || !vmResp.GetFound() || vmResp.GetVm().GetNodeId() == "" || s.peers == nil {
		return ""
	}
	statusResp, err := s.client.Status(ctx, &rpcpb.StatusRequest{})
	if err != nil || statusResp.GetManagerNodeId() == "" || vmResp.GetVm().GetNodeId() == statusResp.GetManagerNodeId() {
		return ""
	}
	return s.peerAddr(vmResp.GetVm().GetNodeId())
}

func (s *Server) createVMSnapshot(ctx context.Context, id, snapshotName string) (*rpcpb.CreateVMSnapshotResponse, error) {
	if addr := s.ownerManagerdAddrForVM(ctx, id); addr != "" {
		return s.peers.CreateVMSnapshot(ctx, addr, id, snapshotName)
	}
	return s.client.CreateVMSnapshot(ctx, &rpcpb.CreateVMSnapshotRequest{Id: id, SnapshotName: snapshotName})
}

func (s *Server) listVMSnapshots(ctx context.Context, id string) (*rpcpb.ListVMSnapshotsResponse, error) {
	if addr := s.ownerManagerdAddrForVM(ctx, id); addr != "" {
		return s.peers.ListVMSnapshots(ctx, addr, id)
	}
	return s.client.ListVMSnapshots(ctx, &rpcpb.ListVMSnapshotsRequest{Id: id})
}

func (s *Server) restoreVMSnapshot(ctx context.Context, id, snapshotName string) (*rpcpb.RestoreVMSnapshotResponse, error) {
	if addr := s.ownerManagerdAddrForVM(ctx, id); addr != "" {
		return s.peers.RestoreVMSnapshot(ctx, addr, id, snapshotName)
	}
	return s.client.RestoreVMSnapshot(ctx, &rpcpb.RestoreVMSnapshotRequest{Id: id, SnapshotName: snapshotName})
}

func (s *Server) deleteVMSnapshot(ctx context.Context, id, snapshotName string) (*rpcpb.DeleteVMSnapshotResponse, error) {
	if addr := s.ownerManagerdAddrForVM(ctx, id); addr != "" {
		return s.peers.DeleteVMSnapshot(ctx, addr, id, snapshotName)
	}
	return s.client.DeleteVMSnapshot(ctx, &rpcpb.DeleteVMSnapshotRequest{Id: id, SnapshotName: snapshotName})
}

// currentVMSnapshots fetches the snapshot list for the VM detail page's
// own panel - a fetch failure is folded into a single message string,
// matching every other "fetch for one detail-page section" helper on
// that page (e.g. resolveSerialLog's own error-message folding).
func (s *Server) currentVMSnapshots(r *http.Request, id string) (names []string, errMsg string) {
	resp, err := s.listVMSnapshots(r.Context(), id)
	if err != nil {
		return nil, err.Error()
	}
	return resp.GetSnapshotNames(), resp.GetError()
}

// handleCreateVMSnapshot/handleRestoreVMSnapshot/handleDeleteVMSnapshot
// all re-render the VM detail page afterward (success or failure),
// mirroring handleSetVMCloudflareExposure's own pattern - the page's
// own Snapshots panel shows whichever error resulted, not a bare error
// page.
func (s *Server) handleCreateVMSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.renderVMPage(w, r, id, "", "", "invalid form: "+err.Error())
		return
	}
	name := strings.TrimSpace(r.FormValue("snapshot_name"))
	resp, err := s.createVMSnapshot(r.Context(), id, name)
	if err != nil {
		s.renderVMPage(w, r, id, "", "", err.Error())
		return
	}
	s.renderVMPage(w, r, id, "", "", resp.GetError())
}

func (s *Server) handleRestoreVMSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	resp, err := s.restoreVMSnapshot(r.Context(), id, name)
	if err != nil {
		s.renderVMPage(w, r, id, "", "", err.Error())
		return
	}
	s.renderVMPage(w, r, id, "", "", resp.GetError())
}

func (s *Server) handleDeleteVMSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	resp, err := s.deleteVMSnapshot(r.Context(), id, name)
	if err != nil {
		s.renderVMPage(w, r, id, "", "", err.Error())
		return
	}
	s.renderVMPage(w, r, id, "", "", resp.GetError())
}
