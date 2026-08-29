package cluster

import (
	"context"
	"errors"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

type fakeVMLister struct {
	resp *internalpb.ListVMsResponse
	err  error
}

func (f *fakeVMLister) ListVMs(context.Context) (*internalpb.ListVMsResponse, error) {
	return f.resp, f.err
}

type fakeDatasetManager struct {
	existing  []string
	created   []string
	listErr   error
	createErr error
}

func (f *fakeDatasetManager) ListDatasets(context.Context) ([]string, error) {
	return f.existing, f.listErr
}

func (f *fakeDatasetManager) CreateDataset(_ context.Context, name string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, name)
	return nil
}

func TestReconciler_RunOnce_CreatesMissingDatasets(t *testing.T) {
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{
			{Id: "vm-1", NodeId: "node-a"},
			{Id: "vm-2", NodeId: "node-b"},
		},
	}}
	zfs := &fakeDatasetManager{}

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.created) != 1 || zfs.created[0] != "vm-1" {
		t.Errorf("created = %v, want [vm-1]", zfs.created)
	}
}

func TestReconciler_RunOnce_FailsWithoutProvisioningOnListVMsError(t *testing.T) {
	raft := &fakeVMLister{err: errors.New("raftd unreachable")}
	zfs := &fakeDatasetManager{}

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want an error when ListVMs fails")
	}
	if len(zfs.created) != 0 {
		t.Errorf("created = %v, want nothing provisioned on a failed fetch", zfs.created)
	}
}

func TestReconciler_RunOnce_FailsWithoutProvisioningOnApplicationError(t *testing.T) {
	// A non-leader raftd returns a normal response with .Error set, not a
	// transport error - RunOnce must treat that the same as a hard
	// failure, not an empty VM list.
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{Error: "not the leader"}}
	zfs := &fakeDatasetManager{}

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want an error when raftd reports a failure")
	}
	if len(zfs.created) != 0 {
		t.Errorf("created = %v, want nothing provisioned when raftd reports a failure", zfs.created)
	}
}

func TestReconciler_RunOnce_PropagatesCreateDatasetError(t *testing.T) {
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	zfs := &fakeDatasetManager{createErr: errors.New("disk full")}

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want the underlying CreateDataset error surfaced")
	}
}
