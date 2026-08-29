package cluster

import (
	"context"
	"fmt"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// vmLister is the subset of *manager.RaftClient the reconciler needs.
// Defined locally (rather than importing internal/manager's concrete
// type into the signature) so tests can supply a fake without any real
// raft/gRPC machinery; *manager.RaftClient satisfies this today without
// any changes on its side.
type vmLister interface {
	ListVMs(ctx context.Context) (*internalpb.ListVMsResponse, error)
}

// datasetManager is the subset of *zfs.Manager the reconciler needs, for
// the same reason as vmLister. *zfs.Manager satisfies this today.
type datasetManager interface {
	ListDatasets(ctx context.Context) ([]string, error)
	CreateDataset(ctx context.Context, name string) error
}

// Reconciler provisions local ZFS storage for VMs assigned to this node,
// based on VMDefinition.node_id in raft's ephemeral state. It only
// creates missing datasets; see Plan's doc comment for why it never
// destroys them.
type Reconciler struct {
	Raft        vmLister
	ZFS         datasetManager
	LocalNodeID string
}

// RunOnce fetches the current VM list, compares it against local
// storage, and provisions whatever's missing for VMs assigned to
// LocalNodeID. It returns an error without provisioning anything if
// either the VM list or the local dataset list can't be fetched -
// reconciling against a partial or failed fetch is exactly the kind of
// mistake Plan's design note warns about avoiding.
func (r *Reconciler) RunOnce(ctx context.Context) error {
	resp, err := r.Raft.ListVMs(ctx)
	if err != nil {
		return fmt.Errorf("cluster: listing VMs: %w", err)
	}
	if resp.GetError() != "" {
		return fmt.Errorf("cluster: listing VMs: %s", resp.GetError())
	}

	desired := make([]VMPlacement, 0, len(resp.GetVms()))
	for _, vm := range resp.GetVms() {
		desired = append(desired, VMPlacement{ID: vm.GetId(), NodeID: vm.GetNodeId()})
	}

	existing, err := r.ZFS.ListDatasets(ctx)
	if err != nil {
		return fmt.Errorf("cluster: listing local datasets: %w", err)
	}

	for _, id := range Plan(desired, existing, r.LocalNodeID) {
		if err := r.ZFS.CreateDataset(ctx, id); err != nil {
			return fmt.Errorf("cluster: creating dataset for VM %s: %w", id, err)
		}
	}
	return nil
}
