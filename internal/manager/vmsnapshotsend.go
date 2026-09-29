// Cross-node VM snapshot transfer (ADR-0090's follow-up, closing the
// gap SHARED.md records against the VM checkpoint RPCs in server.go):
// PushVMSnapshotTo/ReceiveVMSnapshot are the VM-snapshot analogue of
// the jail base-template pair PushJailTemplateTo/ReceiveJailTemplate
// already establish, line for line - same message shape, same error
// style, same peer-only/no-leader-forwarding decision, same
// metadata-then-chunks-then-CloseAndRecv stream discipline, same
// io.Pipe-fed-directly-into-`zfs receive` (never buffered in memory)
// receive handler.
//
// The difference from the jail-template pair is only what travels:
// a jail template is a `zfs send` of a base template dataset received
// into a dataset that does not exist yet on the target, whereas a VM
// snapshot arrives for a VM whose dataset the target usually ALREADY
// holds. That is why the receive here uses `zfs receive -F` (see
// zfs.Manager.ReceiveForce) and why it carries the extra fail-closed
// checks below - a -F receive that fails can leave the destination
// dataset partially received, so "the stream ended" is never treated as
// "the snapshot arrived" on either end.
package manager

import (
	"context"
	"fmt"
	"io"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// vmSnapshotTransfer is the *PeerReporter method PushVMSnapshotTo needs
// to open a client stream on the target node's own ReceiveVMSnapshot
// RPC. PeerForwarder declares it (see server.go), so a real
// *PeerReporter satisfies it and the assertion below holds in
// production; it is still asserted for structurally rather than
// assumed, so a narrower PeerForwarder in a test or a future
// refactor gets a named error naming exactly what is missing rather
// than a success with no transfer behind it.
//
// The client half lives in peer.go beside PushJailTemplate, and its
// production caller is internal/cluster's Reconciler, which asks the
// peer that already holds a named VM checkpoint to push it here before
// cloning from it (ADR-0095's disclosed node-local limitation, closed
// the same way ADR-0089 closed the identical one for jail templates).
type vmSnapshotTransfer interface {
	PushVMSnapshot(ctx context.Context, addr, vmID, snapshotName string, r io.Reader) error
}

// vmSnapshotStore is the *zfs.Manager surface this pair needs beyond
// the quotaSetter subset server.go already declares (which is why it is
// asserted for rather than added there - see vmSnapshotTransfer above
// for the same reasoning). Send/Receive are only reachable through
// ReceiveForce here: a VM snapshot lands on a dataset the target
// already has, so the plain Receive above (which refuses an existing
// destination outright) is never the right primitive for it.
type vmSnapshotStore interface {
	ReceiveForce(ctx context.Context, destName string, r io.Reader) error
	SnapshotExists(ctx context.Context, name string) (bool, error)
}

// validVMSnapshotName is the receiving AND sending end's admission
// check for a snapshot name, deliberately stricter than the ZFS
// grammar actually permits (which also allows "." and ":"): every
// character allowed here is one that cannot be read as a dataset path
// separator, a shell metacharacter, or a leading-dash argument by any
// layer underneath this one, and there is no caller in this project
// that needs more. 64 is ZFS's own maximum snapshot-name component
// length, so nothing legitimate is refused.
//
// This is the same posture validHASTResourceName already takes for a
// resource id (same 1..64 bound, same character class), and the id
// itself is checked through that existing helper rather than a second,
// looser copy of it - see validVMResourceID below.
func validVMSnapshotName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// validVMResourceID admits a bare VM id by running it through the
// existing validHASTResourceName helper with the "vm-" prefix that
// helper already knows, rather than duplicating that character class
// here. A VM's own dataset is named "vm-<id>" (see internal/zfs and
// internal/cluster/hast.go's naming), so this is exactly the check
// that id is safe to interpolate into that dataset name - and
// therefore into a `zfs send`/`zfs receive` argument.
func validVMResourceID(id string) bool {
	return validHASTResourceName("vm-" + id)
}

// PushVMSnapshotTo implements rpcpb.ManagerServiceServer - this node
// (which already holds the named snapshot of id) `zfs send`s it to
// target_node_id via a real ReceiveVMSnapshot client stream. Peer-only
// and never leader-forwarded, for the same reason PushJailTemplateTo
// is: this is a direct node-to-node copy of physical, per-node
// storage state, exactly like the jail-template pair it mirrors, and
// like PushISOTo it is meant to be asked for by a peer that has
// already established this node has the snapshot - never aimed at the
// leader on a caller's behalf.
//
// The snapshot is read from THIS node's own dataset named by id, never
// from anything the caller supplied beyond that id and the snapshot
// name, and both are validated before Send is reached.
func (s *Server) PushVMSnapshotTo(ctx context.Context, req *rpcpb.PushVMSnapshotToRequest) (*rpcpb.PushVMSnapshotToResponse, error) {
	if req.GetId() == "" || req.GetSnapshotName() == "" || req.GetTargetNodeId() == "" {
		return &rpcpb.PushVMSnapshotToResponse{Error: "id, snapshot_name and target_node_id are required"}, nil
	}
	if !validVMResourceID(req.GetId()) || !validVMSnapshotName(req.GetSnapshotName()) {
		return &rpcpb.PushVMSnapshotToResponse{Error: "id or snapshot_name is not a valid VM resource or snapshot name"}, nil
	}
	if s.zfs == nil {
		return &rpcpb.PushVMSnapshotToResponse{Error: "this node has no ZFS support configured"}, nil
	}
	if s.peers == nil {
		return &rpcpb.PushVMSnapshotToResponse{Error: "peer forwarding is not configured on this node"}, nil
	}
	pusher, ok := s.peers.(vmSnapshotTransfer)
	if !ok {
		return &rpcpb.PushVMSnapshotToResponse{Error: "this node's peer forwarder cannot push a VM snapshot (PeerForwarder.PushVMSnapshot is not implemented)"}, nil
	}

	rc, err := s.zfs.Send(ctx, req.GetId()+"@"+req.GetSnapshotName())
	if err != nil {
		return &rpcpb.PushVMSnapshotToResponse{Error: fmt.Sprintf("reading local snapshot %q of VM %q: %v", req.GetSnapshotName(), req.GetId(), err)}, nil
	}
	defer rc.Close()

	targetAddr, err := s.nodeManagerdAddr(ctx, req.GetTargetNodeId())
	if err != nil {
		return &rpcpb.PushVMSnapshotToResponse{Error: err.Error()}, nil
	}

	if err := pusher.PushVMSnapshot(ctx, targetAddr, req.GetId(), req.GetSnapshotName(), rc); err != nil {
		return &rpcpb.PushVMSnapshotToResponse{Error: fmt.Sprintf("pushing to %q: %v", req.GetTargetNodeId(), err)}, nil
	}
	return &rpcpb.PushVMSnapshotToResponse{}, nil
}

// ReceiveVMSnapshot implements rpcpb.ManagerServiceServer - the
// VM-snapshot analogue of ReceiveJailTemplate. The first stream
// message must carry metadata (which VM, which snapshot); every
// message after that carries a chunk of the `zfs send` stream's bytes,
// piped directly into `zfs receive -F` as they arrive, never buffered
// in memory, mirroring ReceiveJailTemplate's own io.Pipe-based handler
// exactly.
//
// Everything that can be refused is refused BEFORE a single byte is
// read into the receive, so a bad request never starts a subprocess
// that would then have to be torn down:
//
//   - id and snapshot_name are validated with the same helpers
//     PushVMSnapshotTo uses. The sender is not trusted to have
//     checked; either end can be the wrong or hostile one.
//   - the VM must exist here. A snapshot for a VM this node has never
//     heard of would otherwise create a dataset nobody can attach to.
//   - a VM this node still holds a dataset for must not be desired to
//     be running, for exactly the reason RestoreVMSnapshot refuses: a
//     -F receive rolls the dataset back and rewrites it underneath the
//     file a bhyve process has open. The "this node still holds it"
//     half is what makes that true - the danger is local, and a VM
//     living entirely on another Comb has no local disk here to
//     rewrite, which is precisely the case this pair exists to serve.
//   - the snapshot must not already exist here. A -F receive over a
//     dataset that already holds a checkpoint of that name would
//     silently replace it, and an operator's earlier checkpoint is
//     exactly the thing a transfer must never quietly destroy.
//
// The fail-closed half is what happens after the receive. Per zfs(8),
// a failed -F receive does not roll back and can leave the dataset
// partially received, so a stream that merely ended proves nothing:
// success is reported only once the snapshot has been CONFIRMED to
// exist locally, and a receive that failed but nevertheless left a
// snapshot of the requested name behind has that snapshot destroyed
// (best effort) so nothing incomplete can go on being listed by
// ListVMSnapshots and then handed to a later RestoreVMSnapshot. This
// is internal/backup/store.go's own posture - the artifact that makes
// something count as complete is written last, and only once
// everything it describes is fully present.
func (s *Server) ReceiveVMSnapshot(stream rpcpb.ManagerService_ReceiveVMSnapshotServer) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("manager: ReceiveVMSnapshot: receiving metadata: %w", err)
	}
	meta := first.GetMetadata()
	if meta == nil {
		return fmt.Errorf("manager: ReceiveVMSnapshot: first message must be metadata")
	}
	if !validVMResourceID(meta.GetId()) || !validVMSnapshotName(meta.GetSnapshotName()) {
		return stream.SendAndClose(&rpcpb.ReceiveVMSnapshotResponse{Error: "id or snapshot_name is not a valid VM resource or snapshot name"})
	}
	if s.zfs == nil {
		return stream.SendAndClose(&rpcpb.ReceiveVMSnapshotResponse{Error: "this node has no ZFS support configured"})
	}
	store, ok := s.zfs.(vmSnapshotStore)
	if !ok {
		return stream.SendAndClose(&rpcpb.ReceiveVMSnapshotResponse{Error: "this node's ZFS store cannot receive a VM snapshot (zfs.Manager.ReceiveForce is not available)"})
	}
	if err := s.vmsnapshotReceivePreconditions(stream.Context(), meta); err != nil {
		return stream.SendAndClose(&rpcpb.ReceiveVMSnapshotResponse{Error: err.Error()})
	}

	ctx := stream.Context()
	dest := meta.GetId()
	snap := dest + "@" + meta.GetSnapshotName()

	pr, pw := io.Pipe()
	recvDone := make(chan error, 1)
	go func() {
		recvErr := store.ReceiveForce(ctx, dest, pr)
		pr.CloseWithError(recvErr)
		recvDone <- recvErr
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			pw.Close()
			break
		}
		if err != nil {
			pw.CloseWithError(err)
			<-recvDone
			return fmt.Errorf("manager: ReceiveVMSnapshot: receiving chunk: %w", err)
		}
		if _, err := pw.Write(req.GetChunk()); err != nil {
			break // ReceiveForce's goroutine already failed; its error is reported below
		}
	}

	if recvErr := <-recvDone; recvErr != nil {
		return stream.SendAndClose(&rpcpb.ReceiveVMSnapshotResponse{
			Error: s.discardPartialVMSnapshot(ctx, store, snap, recvErr.Error()),
		})
	}

	// Written last, and only once every byte of the stream is known to
	// have landed: the snapshot is the thing that makes this transfer
	// count as complete, so it is verified to exist before the response
	// claims it does.
	exists, err := store.SnapshotExists(ctx, snap)
	if err != nil {
		return stream.SendAndClose(&rpcpb.ReceiveVMSnapshotResponse{Error: fmt.Sprintf("confirming received snapshot %q: %v", snap, err)})
	}
	if !exists {
		return stream.SendAndClose(&rpcpb.ReceiveVMSnapshotResponse{Error: fmt.Sprintf("receive reported success but snapshot %q is not present", snap)})
	}
	return stream.SendAndClose(&rpcpb.ReceiveVMSnapshotResponse{Id: meta.GetId(), SnapshotName: meta.GetSnapshotName()})
}

// vmsnapshotReceivePreconditions is the refuse-before-touching-anything
// half of ReceiveVMSnapshot's fail-closed posture. Each check is best
// effort in exactly the way RestoreVMSnapshot's own running-VM check is
// (a nil raft client, or a failed/leader-forwarded GetVM, skips the
// check rather than blocking the transfer) - except the snapshot-exists
// check, which is not best effort at all: it is a direct query against
// this node's own ZFS and a failure there means the receive is refused,
// because proceeding would mean overwriting a checkpoint the caller
// never said they were replacing.
func (s *Server) vmsnapshotReceivePreconditions(ctx context.Context, meta *rpcpb.VMSnapshotMetadata) error {
	if s.raft != nil {
		vmResp, err := s.GetVM(ctx, &rpcpb.GetVMRequest{Id: meta.GetId()})
		if err == nil && !vmResp.GetFound() {
			return fmt.Errorf("VM %q has no replicated definition on this node, so a snapshot for it would create a dataset nothing could ever attach to", meta.GetId())
		}
		// The running check is about a LOCAL disk, not about the
		// cluster-wide desired state GetVM reports. A `-F` receive is
		// only dangerous where it rewrites a dataset a bhyve process on
		// THIS node has open, and a VM this node has never held has no
		// such process and no such dataset - `zfs receive -F` would
		// create the dataset, not roll one back. Refusing on the
		// replicated desired state alone would make the cross-Comb
		// clone-source fetch impossible for every source VM that is
		// running on its own Comb, which is the ordinary case, while
		// protecting nothing: the running VM's disk is on the other
		// node. The local-dataset half is what actually closes the
		// window it was written for, including the migration window
		// where this node's raft view has already moved the VM
		// elsewhere but its bhyve process and dataset are still here.
		if err == nil && vmResp.GetVm().GetDesiredState() == rpcpb.VMState_VM_STATE_RUNNING {
			held, heldErr := s.zfs.DatasetExists(ctx, meta.GetId())
			if heldErr == nil && held {
				return fmt.Errorf("VM %q must be stopped before receiving a snapshot for it: this node still holds its dataset", meta.GetId())
			}
		}
	}
	store, ok := s.zfs.(vmSnapshotStore)
	if !ok {
		return fmt.Errorf("this node's ZFS store cannot receive a VM snapshot")
	}
	exists, err := store.SnapshotExists(ctx, meta.GetId()+"@"+meta.GetSnapshotName())
	if err != nil {
		return fmt.Errorf("checking for an existing local snapshot of VM %q: %w", meta.GetId(), err)
	}
	if exists {
		return fmt.Errorf("VM %q already has a snapshot named %q on this node", meta.GetId(), meta.GetSnapshotName())
	}
	return nil
}

// discardPartialVMSnapshot is what a failed receive leaves behind,
// cleaned up: the snapshot of exactly the requested name that a
// partial `zfs receive -F` may nonetheless have published. It can only
// be something this transfer created - ReceiveVMSnapshot refuses
// outright when that snapshot is already there - so removing it cannot
// cost an operator an earlier checkpoint. The destroy is best effort
// because ZFS itself refuses to destroy snapshots on a dataset with an
// unfinished receive; when it does, that is reported in the error text
// rather than hidden, since it means the dataset needs operator
// attention (`zfs receive -A`) and this node will not pretend
// otherwise.
func (s *Server) discardPartialVMSnapshot(ctx context.Context, store vmSnapshotStore, snap, recvErr string) string {
	exists, err := store.SnapshotExists(ctx, snap)
	if err != nil || !exists {
		return recvErr
	}
	if err := s.zfs.DestroySnapshot(ctx, snap); err != nil {
		return fmt.Sprintf("%s; the partially received snapshot %q could not be removed (%v), so this dataset needs operator attention", recvErr, snap, err)
	}
	return fmt.Sprintf("%s; the partially received snapshot %q was removed", recvErr, snap)
}
