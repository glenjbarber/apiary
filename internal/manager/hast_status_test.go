package manager

import (
	"context"
	"errors"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hast"
)

type fakeHASTStatus struct {
	name  string
	value *hast.Status
	err   error
}

func (f *fakeHASTStatus) Status(_ context.Context, name string) (*hast.Status, error) {
	f.name = name
	return f.value, f.err
}

func TestGetLocalHASTResourceStatusReportsLocalObservation(t *testing.T) {
	reader := &fakeHASTStatus{value: &hast.Status{Role: "primary", ResourceStatus: "complete", Replication: "memsync", Dirty: "0 (0B)", ExtentSize: "2.0MB"}}
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetHASTStatusReader(reader)

	resp, err := s.GetLocalHASTResourceStatus(context.Background(), &rpcpb.GetLocalHASTResourceStatusRequest{ResourceName: "vm-test_1"})
	if err != nil {
		t.Fatalf("GetLocalHASTResourceStatus() error: %v", err)
	}
	if resp.GetError() != "" || reader.name != "vm-test_1" || resp.GetRole() != "primary" || resp.GetResourceStatus() != "complete" || resp.GetReplication() != "memsync" || resp.GetDirty() != "0 (0B)" || resp.GetExtentSize() != "2.0MB" || resp.GetObservedAtUnix() == 0 {
		t.Errorf("response = %+v; queried name %q", resp, reader.name)
	}
}

func TestGetLocalHASTResourceStatusRejectsInvalidNames(t *testing.T) {
	reader := &fakeHASTStatus{}
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetHASTStatusReader(reader)
	for _, name := range []string{"", "vm-", "vm-../x", "jail-a/b", "disk-test", "vm-" + string(make([]byte, 65))} {
		resp, err := s.GetLocalHASTResourceStatus(context.Background(), &rpcpb.GetLocalHASTResourceStatusRequest{ResourceName: name})
		if err != nil || resp.GetError() == "" {
			t.Errorf("name %q: response=%+v err=%v, want validation error", name, resp, err)
		}
	}
	if reader.name != "" {
		t.Errorf("reader was called for invalid name %q", reader.name)
	}
}

func TestGetLocalHASTResourceStatusSurfacesLocalErrors(t *testing.T) {
	reader := &fakeHASTStatus{err: errors.New("hastctl unavailable")}
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetHASTStatusReader(reader)
	resp, err := s.GetLocalHASTResourceStatus(context.Background(), &rpcpb.GetLocalHASTResourceStatusRequest{ResourceName: "jail-test"})
	if err != nil || resp.GetError() != "hastctl unavailable" || resp.GetObservedAtUnix() == 0 {
		t.Errorf("response=%+v err=%v, want timestamped local error", resp, err)
	}
}
