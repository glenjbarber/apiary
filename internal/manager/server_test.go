package manager

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hoststats"
	"github.com/glenjbarber/apiary/internal/isostore"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// fakeISOManager is a fake isoManager for testing Server's ISO RPCs
// without any real file I/O.
type fakeISOManager struct {
	savedName string
	savedData []byte
	savedHash string
	saveErr   error

	listInfos []isostore.Info
	listErr   error

	deletedName string
	deleteErr   error

	pathFor map[string]string
	pathErr error
}

func (f *fakeISOManager) Save(name string, r io.Reader, expectedSHA256 string) (*isostore.Info, error) {
	if f.saveErr != nil {
		return nil, f.saveErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f.savedName = name
	f.savedData = data
	f.savedHash = expectedSHA256
	return &isostore.Info{Name: name, SizeBytes: int64(len(data)), SHA256: expectedSHA256}, nil
}

func (f *fakeISOManager) List() ([]isostore.Info, error) {
	return f.listInfos, f.listErr
}

func (f *fakeISOManager) Delete(name string) error {
	f.deletedName = name
	return f.deleteErr
}

func (f *fakeISOManager) Path(name string) (string, bool, error) {
	if f.pathErr != nil {
		return "", false, f.pathErr
	}
	if f.pathFor != nil {
		if p, ok := f.pathFor[name]; ok {
			return p, true, nil
		}
	}
	return "", false, nil
}

// fakeUploadStream drives Server.UploadISO without any real gRPC
// connection - it replays a fixed sequence of client messages via Recv
// and records whatever Server sends via SendAndClose.
type fakeUploadStream struct {
	grpc.ServerStream
	reqs []*rpcpb.UploadISORequest
	idx  int
	resp *rpcpb.UploadISOResponse
}

func (f *fakeUploadStream) Recv() (*rpcpb.UploadISORequest, error) {
	if f.idx >= len(f.reqs) {
		return nil, io.EOF
	}
	req := f.reqs[f.idx]
	f.idx++
	return req, nil
}

func (f *fakeUploadStream) SendAndClose(resp *rpcpb.UploadISOResponse) error {
	f.resp = resp
	return nil
}

func metadataMsg(name, hash string) *rpcpb.UploadISORequest {
	return &rpcpb.UploadISORequest{Data: &rpcpb.UploadISORequest_Metadata{
		Metadata: &rpcpb.ISOUploadMetadata{Name: name, ExpectedSha256: hash},
	}}
}

func chunkMsg(data string) *rpcpb.UploadISORequest {
	return &rpcpb.UploadISORequest{Data: &rpcpb.UploadISORequest_Chunk{Chunk: []byte(data)}}
}

func TestServer_ImageInventoryObservations_DistinguishesObservedAndUnknown(t *testing.T) {
	s := NewServer(nil, "node-a", &fakeISOManager{listInfos: []isostore.Info{{Name: "ubuntu.raw"}}}, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	servers := []*internalpb.ServerInfo{
		{Id: "node-a", Address: "10.0.0.1:17600"},
		{Id: "node-b", Address: "10.0.0.2:17600"},
		{Id: "failed", Address: "10.0.0.3:17600"},
	}

	got := s.imageInventoryObservations(context.Background(), servers, "failed", "node-a")
	if len(got) != 2 {
		t.Fatalf("imageInventoryObservations() = %+v, want 2 remaining Hives", got)
	}
	if !got[0].Observed || len(got[0].Names) != 1 || got[0].Names[0] != "ubuntu.raw" {
		t.Errorf("local observation = %+v, want observed ubuntu.raw", got[0])
	}
	if got[1].Observed {
		t.Errorf("remote observation = %+v, want unknown without peer forwarding", got[1])
	}
}

func TestServer_UploadISO_StreamsChunksIntoStore(t *testing.T) {
	isos := &fakeISOManager{}
	s := NewServer(nil, "node-1", isos, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	stream := &fakeUploadStream{reqs: []*rpcpb.UploadISORequest{
		metadataMsg("test.iso", "deadbeef"),
		chunkMsg("hello "),
		chunkMsg("world"),
	}}

	if err := s.UploadISO(stream); err != nil {
		t.Fatalf("UploadISO() error: %v", err)
	}
	if string(isos.savedData) != "hello world" {
		t.Errorf("saved data = %q, want %q", isos.savedData, "hello world")
	}
	if isos.savedName != "test.iso" || isos.savedHash != "deadbeef" {
		t.Errorf("saved name/hash = %q/%q, want test.iso/deadbeef", isos.savedName, isos.savedHash)
	}
	if stream.resp.GetError() != "" {
		t.Errorf("response error = %q, want empty", stream.resp.GetError())
	}
	if stream.resp.GetName() != "test.iso" {
		t.Errorf("response name = %q, want test.iso", stream.resp.GetName())
	}
}

func TestServer_UploadISO_MissingMetadataFirstIsError(t *testing.T) {
	s := NewServer(nil, "node-1", &fakeISOManager{}, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	stream := &fakeUploadStream{reqs: []*rpcpb.UploadISORequest{chunkMsg("oops")}}

	if err := s.UploadISO(stream); err == nil {
		t.Fatalf("UploadISO() = nil error, want rejection when metadata isn't first")
	}
}

func TestServer_UploadISO_SaveErrorReportedInResponse(t *testing.T) {
	isos := &fakeISOManager{saveErr: errors.New("sha256 mismatch")}
	s := NewServer(nil, "node-1", isos, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	stream := &fakeUploadStream{reqs: []*rpcpb.UploadISORequest{
		metadataMsg("test.iso", "wronghash"),
		chunkMsg("data"),
	}}

	if err := s.UploadISO(stream); err != nil {
		t.Fatalf("UploadISO() error: %v, want a response-level error instead", err)
	}
	if stream.resp.GetError() == "" {
		t.Errorf("response error = empty, want the save error surfaced")
	}
}

func TestServer_ListISOs(t *testing.T) {
	isos := &fakeISOManager{listInfos: []isostore.Info{
		{Name: "a.iso", SizeBytes: 100, SHA256: "aaa"},
		{Name: "b.iso", SizeBytes: 200, SHA256: "bbb"},
	}}
	s := NewServer(nil, "node-1", isos, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.ListISOs(context.Background(), &rpcpb.ListISOsRequest{})
	if err != nil {
		t.Fatalf("ListISOs() error: %v", err)
	}
	if len(resp.GetIsos()) != 2 || resp.GetIsos()[0].GetName() != "a.iso" {
		t.Errorf("ListISOs() = %+v, want 2 entries starting with a.iso", resp.GetIsos())
	}
}

func TestServer_ListISOs_ErrorSurfacedInResponse(t *testing.T) {
	isos := &fakeISOManager{listErr: errors.New("disk error")}
	s := NewServer(nil, "node-1", isos, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.ListISOs(context.Background(), &rpcpb.ListISOsRequest{})
	if err != nil {
		t.Fatalf("ListISOs() error: %v, want a response-level error instead", err)
	}
	if resp.GetError() == "" {
		t.Errorf("response error = empty, want the underlying error surfaced")
	}
}

func TestServer_DeleteISO(t *testing.T) {
	isos := &fakeISOManager{}
	s := NewServer(nil, "node-1", isos, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.DeleteISO(context.Background(), &rpcpb.DeleteISORequest{Name: "old.iso"})
	if err != nil {
		t.Fatalf("DeleteISO() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Errorf("response error = %q, want empty", resp.GetError())
	}
	if isos.deletedName != "old.iso" {
		t.Errorf("deletedName = %q, want old.iso", isos.deletedName)
	}
}

func TestServer_HostStats(t *testing.T) {
	s := NewServer(nil, "node-1", &fakeISOManager{}, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.statsGather = func(context.Context) *hoststats.Snapshot {
		return &hoststats.Snapshot{
			CPU:    hoststats.CPUInfo{Cores: 4, LoadAvg1: 1.5},
			Mem:    hoststats.MemInfo{TotalBytes: 1000, FreeBytes: 200},
			Pools:  []hoststats.PoolInfo{{Name: "zroot", SizeBytes: 500, Health: "ONLINE"}},
			Disks:  []hoststats.DiskInfo{{Name: "ada0", Healthy: true}},
			Net:    []hoststats.NetIface{{Name: "re0", RxBytes: 10, TxBytes: 20}},
			Errors: []string{"disks: partial failure"},
		}
	}

	resp, err := s.HostStats(context.Background(), &rpcpb.HostStatsRequest{})
	if err != nil {
		t.Fatalf("HostStats() error: %v", err)
	}
	if resp.GetNodeId() != "node-1" {
		t.Errorf("NodeId = %q, want node-1", resp.GetNodeId())
	}
	if resp.GetCpu().GetCores() != 4 || resp.GetCpu().GetLoadAvg_1() != 1.5 {
		t.Errorf("Cpu = %+v, want Cores=4 LoadAvg_1=1.5", resp.GetCpu())
	}
	if resp.GetMem().GetTotalBytes() != 1000 || resp.GetMem().GetFreeBytes() != 200 {
		t.Errorf("Mem = %+v, want TotalBytes=1000 FreeBytes=200", resp.GetMem())
	}
	if len(resp.GetPools()) != 1 || resp.GetPools()[0].GetName() != "zroot" {
		t.Errorf("Pools = %+v, want [zroot]", resp.GetPools())
	}
	if len(resp.GetDisks()) != 1 || !resp.GetDisks()[0].GetHealthy() {
		t.Errorf("Disks = %+v, want [ada0 healthy]", resp.GetDisks())
	}
	if len(resp.GetNet()) != 1 || resp.GetNet()[0].GetRxBytes() != 10 {
		t.Errorf("Net = %+v, want [re0 rx=10]", resp.GetNet())
	}
	if len(resp.GetErrors()) != 1 || resp.GetErrors()[0] != "disks: partial failure" {
		t.Errorf("Errors = %v, want [disks: partial failure]", resp.GetErrors())
	}
}

// fakeReconcilerStats is a fake reconcilerStats for testing HostStats's
// new reconcile fields (ADR-0056) without a real internal/cluster.Reconciler.
type fakeReconcilerStats struct {
	attempt   time.Time
	attemptOK bool
	success   time.Time
	successOK bool
	interval  time.Duration
}

func (f *fakeReconcilerStats) LastReconcileAttempt() (time.Time, bool) { return f.attempt, f.attemptOK }
func (f *fakeReconcilerStats) LastReconcileSuccess() (time.Time, bool) { return f.success, f.successOK }
func (f *fakeReconcilerStats) ReconcileInterval() time.Duration        { return f.interval }

func TestHostStats_ReconcileFieldsZeroWhenReconcilerNil(t *testing.T) {
	s := NewServer(nil, "node-1", &fakeISOManager{}, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.statsGather = func(context.Context) *hoststats.Snapshot { return &hoststats.Snapshot{} }

	resp, err := s.HostStats(context.Background(), &rpcpb.HostStatsRequest{})
	if err != nil {
		t.Fatalf("HostStats() error: %v", err)
	}
	if resp.GetLastReconcileSuccessUnix() != 0 || resp.GetLastReconcileAttemptUnix() != 0 || resp.GetReconcileIntervalSeconds() != 0 {
		t.Errorf("reconcile fields = %+v, want all-zero with a nil reconciler", resp)
	}
}

func TestHostStats_ReconcileFieldsPopulatedFromReconciler(t *testing.T) {
	attempt := time.Unix(2000, 0)
	success := time.Unix(1000, 0)
	fake := &fakeReconcilerStats{attempt: attempt, attemptOK: true, success: success, successOK: true, interval: 45 * time.Second}
	s := NewServer(nil, "node-1", &fakeISOManager{}, nil, nil, nil, nil, "", nil, nil, nil, 0, fake)
	s.statsGather = func(context.Context) *hoststats.Snapshot { return &hoststats.Snapshot{} }

	resp, err := s.HostStats(context.Background(), &rpcpb.HostStatsRequest{})
	if err != nil {
		t.Fatalf("HostStats() error: %v", err)
	}
	if resp.GetLastReconcileSuccessUnix() != success.Unix() {
		t.Errorf("LastReconcileSuccessUnix = %d, want %d", resp.GetLastReconcileSuccessUnix(), success.Unix())
	}
	if resp.GetLastReconcileAttemptUnix() != attempt.Unix() {
		t.Errorf("LastReconcileAttemptUnix = %d, want %d", resp.GetLastReconcileAttemptUnix(), attempt.Unix())
	}
	if resp.GetReconcileIntervalSeconds() != 45 {
		t.Errorf("ReconcileIntervalSeconds = %d, want 45", resp.GetReconcileIntervalSeconds())
	}
}

// fakeNodeConfigStore is a fake nodeConfigStore, without any real file
// I/O involved.
type fakeNodeConfigStore struct {
	cfg      nodeconfig.Config
	loadErr  error
	saveErr  error
	lastSave nodeconfig.Config
}

func (f *fakeNodeConfigStore) Load() (nodeconfig.Config, error) {
	if f.loadErr != nil {
		return nodeconfig.Config{}, f.loadErr
	}
	return f.cfg, nil
}

func (f *fakeNodeConfigStore) Save(cfg nodeconfig.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.lastSave = cfg
	return nil
}

func TestServer_GetNodeConfig(t *testing.T) {
	store := &fakeNodeConfigStore{cfg: nodeconfig.Config{Uplink: "re0", NATUplink: "bridge0"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)

	resp, err := s.GetNodeConfig(context.Background(), &rpcpb.GetNodeConfigRequest{})
	if err != nil {
		t.Fatalf("GetNodeConfig() error: %v", err)
	}
	if resp.GetUplink() != "re0" || resp.GetNatUplink() != "bridge0" {
		t.Errorf("GetNodeConfig() = %+v, want Uplink=re0 NatUplink=bridge0", resp)
	}
}

func TestServer_GetNodeConfig_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.GetNodeConfig(context.Background(), &rpcpb.GetNodeConfigRequest{})
	if err != nil {
		t.Fatalf("GetNodeConfig() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("GetNodeConfig() error field = empty, want a not-configured message")
	}
}

func TestServer_UpdateNodeConfig(t *testing.T) {
	store := &fakeNodeConfigStore{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)

	resp, err := s.UpdateNodeConfig(context.Background(), &rpcpb.UpdateNodeConfigRequest{Uplink: "em0", NatUplink: "em0"})
	if err != nil {
		t.Fatalf("UpdateNodeConfig() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("UpdateNodeConfig() returned error: %s", resp.GetError())
	}
	if store.lastSave.Uplink != "em0" || store.lastSave.NATUplink != "em0" {
		t.Errorf("saved config = %+v, want Uplink=em0 NATUplink=em0", store.lastSave)
	}
}

// fakeQuotaSetter is a fake quotaSetter, without any real zfs(8) binary
// involved.
type fakeQuotaSetter struct {
	err          error
	lastName     string
	lastProperty string
	lastValue    string
}

func (f *fakeQuotaSetter) SetProperty(_ context.Context, name, prop, value string) error {
	if f.err != nil {
		return f.err
	}
	f.lastName, f.lastProperty, f.lastValue = name, prop, value
	return nil
}

func TestServer_SetDatasetQuota(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	resp, err := s.SetDatasetQuota(context.Background(), &rpcpb.SetDatasetQuotaRequest{DatasetName: "vm-1", Quota: "10G"})
	if err != nil {
		t.Fatalf("SetDatasetQuota() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("SetDatasetQuota() returned error: %s", resp.GetError())
	}
	if zfsMgr.lastName != "vm-1" || zfsMgr.lastProperty != "quota" || zfsMgr.lastValue != "10G" {
		t.Errorf("SetProperty called with (%q, %q, %q), want (vm-1, quota, 10G)", zfsMgr.lastName, zfsMgr.lastProperty, zfsMgr.lastValue)
	}
}

func TestServer_SetDatasetQuota_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.SetDatasetQuota(context.Background(), &rpcpb.SetDatasetQuotaRequest{DatasetName: "vm-1", Quota: "10G"})
	if err != nil {
		t.Fatalf("SetDatasetQuota() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("SetDatasetQuota() error field = empty, want a not-configured message")
	}
}
