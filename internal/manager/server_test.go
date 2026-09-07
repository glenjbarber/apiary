package manager

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/assumptionregister"
	"github.com/glenjbarber/apiary/internal/hoststats"
	"github.com/glenjbarber/apiary/internal/isostore"
	"github.com/glenjbarber/apiary/internal/netif"
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
	attempt              time.Time
	attemptOK            bool
	success              time.Time
	successOK            bool
	interval             time.Duration
	cloudflareConfigured bool
}

type fakeAssumptionRegister struct {
	claims []assumptionregister.Claim
}

func (f *fakeAssumptionRegister) List() ([]assumptionregister.Claim, error) { return f.claims, nil }
func (f *fakeAssumptionRegister) Save(c assumptionregister.Claim, _ time.Time) error {
	f.claims = append(f.claims, c)
	return nil
}
func (f *fakeAssumptionRegister) Delete(id string) error {
	for i, claim := range f.claims {
		if claim.ID == id {
			f.claims = append(f.claims[:i], f.claims[i+1:]...)
			return nil
		}
	}
	return errors.New("not found")
}

func TestServer_AssumptionRegisterAPILocalOnly(t *testing.T) {
	register := &fakeAssumptionRegister{claims: []assumptionregister.Claim{{
		ID: "claim-1", Statement: "route is stable", Owner: "ops", Scope: "hive:node-1",
		Evidence: "manual check", ExpiresAt: time.Now().Add(time.Hour),
	}}}
	s := NewServer(nil, "node-1", &fakeISOManager{}, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetAssumptionRegister(register)
	resp, err := s.ListAssumptionClaims(context.Background(), &rpcpb.ListAssumptionClaimsRequest{})
	if err != nil || len(resp.GetClaims()) != 1 || resp.GetClaims()[0].GetId() != "claim-1" {
		t.Fatalf("ListAssumptionClaims() = %+v, %v", resp, err)
	}
	result, err := s.SaveAssumptionClaim(context.Background(), &rpcpb.SaveAssumptionClaimRequest{Claim: &rpcpb.AssumptionClaim{
		Id: "claim-2", Statement: "peer route", Owner: "ops", Scope: "colony", Evidence: "ticket", ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	}})
	if err != nil || result.GetError() != "" || len(register.claims) != 2 {
		t.Fatalf("SaveAssumptionClaim() = %+v, %v; claims=%+v", result, err, register.claims)
	}
}

func (f *fakeReconcilerStats) LastReconcileAttempt() (time.Time, bool) { return f.attempt, f.attemptOK }
func (f *fakeReconcilerStats) LastReconcileSuccess() (time.Time, bool) { return f.success, f.successOK }
func (f *fakeReconcilerStats) ReconcileInterval() time.Duration        { return f.interval }
func (f *fakeReconcilerStats) CloudflareConfigured() bool              { return f.cloudflareConfigured }

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
	if resp.GetCloudflareConfigured() {
		t.Errorf("CloudflareConfigured = true, want false with a nil reconciler")
	}
}

func TestHostStats_CloudflareConfiguredReflectsReconciler(t *testing.T) {
	fake := &fakeReconcilerStats{cloudflareConfigured: true}
	s := NewServer(nil, "node-1", &fakeISOManager{}, nil, nil, nil, nil, "", nil, nil, nil, 0, fake)
	s.statsGather = func(context.Context) *hoststats.Snapshot { return &hoststats.Snapshot{} }

	resp, err := s.HostStats(context.Background(), &rpcpb.HostStatsRequest{})
	if err != nil {
		t.Fatalf("HostStats() error: %v", err)
	}
	if !resp.GetCloudflareConfigured() {
		t.Errorf("CloudflareConfigured = false, want true when the reconciler reports it configured")
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

type fakeNodeServiceController struct {
	services    []*rpcpb.NodeService
	listErr     error
	restartErr  error
	restartName string
}

func (f *fakeNodeServiceController) List(context.Context) ([]*rpcpb.NodeService, error) {
	return f.services, f.listErr
}

func (f *fakeNodeServiceController) Restart(_ context.Context, name string) error {
	f.restartName = name
	return f.restartErr
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
	store := &fakeNodeConfigStore{cfg: nodeconfig.Config{Uplink: "re0", NATUplink: "bridge0", DNSServer: "10.62.0.1", JailEnabled: boolPtr(true)}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
	s.SetNetworkInterfaceLister(func() ([]netif.Interface, error) {
		return []netif.Interface{
			{Name: "bridge0", Up: true, Addresses: []string{"10.50.0.14/24"}},
			{Name: "re0", Up: false},
		}, nil
	})

	resp, err := s.GetNodeConfig(context.Background(), &rpcpb.GetNodeConfigRequest{})
	if err != nil {
		t.Fatalf("GetNodeConfig() error: %v", err)
	}
	if resp.GetUplink() != "re0" || resp.GetNatUplink() != "bridge0" || resp.GetDhcpDnsServer() != "10.62.0.1" || !resp.GetJailEnabled() || resp.JailEnabled == nil {
		t.Errorf("GetNodeConfig() = %+v, want Uplink=re0 NatUplink=bridge0 DhcpDnsServer=10.62.0.1 JailEnabled=true", resp)
	}
	if len(resp.GetAvailableInterfaces()) != 2 || resp.GetAvailableInterfaces()[0].GetName() != "bridge0" || !resp.GetAvailableInterfaces()[0].GetUp() {
		t.Errorf("GetNodeConfig() interfaces = %+v, want bridge0 up and re0 down", resp.GetAvailableInterfaces())
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

func TestServer_GetNodeConfig_InterfaceInventoryErrorPreservesConfig(t *testing.T) {
	store := &fakeNodeConfigStore{cfg: nodeconfig.Config{Uplink: "bridge999"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
	s.SetNetworkInterfaceLister(func() ([]netif.Interface, error) {
		return nil, errors.New("interface enumeration failed")
	})

	resp, err := s.GetNodeConfig(context.Background(), &rpcpb.GetNodeConfigRequest{})
	if err != nil {
		t.Fatalf("GetNodeConfig() error: %v", err)
	}
	if resp.GetUplink() != "bridge999" || resp.GetInterfaceInventoryError() != "interface enumeration failed" {
		t.Errorf("GetNodeConfig() = %+v, want saved uplink and inventory error", resp)
	}
}

func TestServer_UpdateNodeConfig(t *testing.T) {
	store := &fakeNodeConfigStore{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)

	resp, err := s.UpdateNodeConfig(context.Background(), &rpcpb.UpdateNodeConfigRequest{Uplink: "em0", NatUplink: "em0", DhcpDnsServer: "10.62.0.1", JailEnabled: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateNodeConfig() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("UpdateNodeConfig() returned error: %s", resp.GetError())
	}
	if store.lastSave.Uplink != "em0" || store.lastSave.NATUplink != "em0" || store.lastSave.DNSServer != "10.62.0.1" || store.lastSave.JailEnabled == nil || !*store.lastSave.JailEnabled {
		t.Errorf("saved config = %+v, want Uplink=em0 NATUplink=em0 DNSServer=10.62.0.1 JailEnabled=true", store.lastSave)
	}
}

// TestServer_GetNodeConfig_NeverReturnsSecrets is the regression test
// for ADR-0070's write-only secret design: PeerAPIKey/RaftdToken must
// never appear anywhere in a GetNodeConfig response, only whether one
// is currently set.
func TestServer_GetNodeConfig_NeverReturnsSecrets(t *testing.T) {
	store := &fakeNodeConfigStore{cfg: nodeconfig.Config{PeerAPIKey: "apk_supersecret", RaftdToken: "raftd_supersecret"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)

	resp, err := s.GetNodeConfig(context.Background(), &rpcpb.GetNodeConfigRequest{})
	if err != nil {
		t.Fatalf("GetNodeConfig() error: %v", err)
	}
	if !resp.GetPeerApiKeySet() || !resp.GetRaftdTokenSet() {
		t.Errorf("GetNodeConfig() PeerApiKeySet/RaftdTokenSet = %v/%v, want true/true", resp.GetPeerApiKeySet(), resp.GetRaftdTokenSet())
	}
	body, err := (protojson.MarshalOptions{}).Marshal(resp)
	if err != nil {
		t.Fatalf("marshaling response: %v", err)
	}
	if strings.Contains(string(body), "supersecret") {
		t.Errorf("GetNodeConfig() response leaked a secret value: %s", body)
	}
}

// TestServer_UpdateNodeConfig_SecretSemantics covers the three-way
// write-only behavior an empty request field must NOT accidentally
// clear a previously-saved secret (mirroring the Users page's own
// "leave blank to keep current" password-change convention), a
// non-empty value sets a new one, and the explicit clear flag actually
// clears it.
func TestServer_UpdateNodeConfig_SecretSemantics(t *testing.T) {
	t.Run("empty leaves current value unchanged", func(t *testing.T) {
		store := &fakeNodeConfigStore{cfg: nodeconfig.Config{PeerAPIKey: "apk_existing", RaftdToken: "raftd_existing"}}
		s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
		if _, err := s.UpdateNodeConfig(context.Background(), &rpcpb.UpdateNodeConfigRequest{}); err != nil {
			t.Fatalf("UpdateNodeConfig() error: %v", err)
		}
		if store.lastSave.PeerAPIKey != "apk_existing" || store.lastSave.RaftdToken != "raftd_existing" {
			t.Errorf("saved secrets = %+v, want both left unchanged", store.lastSave)
		}
	})
	t.Run("non-empty sets a new value", func(t *testing.T) {
		store := &fakeNodeConfigStore{cfg: nodeconfig.Config{PeerAPIKey: "apk_old"}}
		s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
		if _, err := s.UpdateNodeConfig(context.Background(), &rpcpb.UpdateNodeConfigRequest{PeerApiKey: "apk_new"}); err != nil {
			t.Fatalf("UpdateNodeConfig() error: %v", err)
		}
		if store.lastSave.PeerAPIKey != "apk_new" {
			t.Errorf("saved PeerAPIKey = %q, want apk_new", store.lastSave.PeerAPIKey)
		}
	})
	t.Run("explicit clear flag clears it", func(t *testing.T) {
		store := &fakeNodeConfigStore{cfg: nodeconfig.Config{PeerAPIKey: "apk_old", RaftdToken: "raftd_old"}}
		s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
		if _, err := s.UpdateNodeConfig(context.Background(), &rpcpb.UpdateNodeConfigRequest{ClearPeerApiKey: true, ClearRaftdToken: true}); err != nil {
			t.Fatalf("UpdateNodeConfig() error: %v", err)
		}
		if store.lastSave.PeerAPIKey != "" || store.lastSave.RaftdToken != "" {
			t.Errorf("saved secrets = %+v, want both cleared", store.lastSave)
		}
	})
}

// TestServer_UpdateNodeConfig_InvalidDurationRejectedBeforeSave proves
// a malformed duration field is rejected with a clear error and never
// reaches nodeconfig.Save at all - not persisted only to fail the next
// time managerd actually starts.
func TestServer_UpdateNodeConfig_InvalidDurationRejectedBeforeSave(t *testing.T) {
	store := &fakeNodeConfigStore{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)

	resp, err := s.UpdateNodeConfig(context.Background(), &rpcpb.UpdateNodeConfigRequest{ReconcileInterval: "not-a-duration"})
	if err != nil {
		t.Fatalf("UpdateNodeConfig() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("UpdateNodeConfig() error field = empty, want a rejection")
	}
	if store.lastSave != (nodeconfig.Config{}) {
		t.Errorf("Save() was called with %+v despite the invalid duration, want it never called", store.lastSave)
	}
}

// TestServer_UpdateNodeConfig_ParsesDurationFields confirms a
// well-formed duration string round-trips into the correct
// time.Duration value.
func TestServer_UpdateNodeConfig_ParsesDurationFields(t *testing.T) {
	store := &fakeNodeConfigStore{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)

	resp, err := s.UpdateNodeConfig(context.Background(), &rpcpb.UpdateNodeConfigRequest{ReconcileInterval: "45s", AssumptionCheckInterval: "2m"})
	if err != nil {
		t.Fatalf("UpdateNodeConfig() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("UpdateNodeConfig() returned error: %s", resp.GetError())
	}
	if store.lastSave.ReconcileInterval != 45*time.Second {
		t.Errorf("saved ReconcileInterval = %v, want 45s", store.lastSave.ReconcileInterval)
	}
	if store.lastSave.AssumptionCheckInterval != 2*time.Minute {
		t.Errorf("saved AssumptionCheckInterval = %v, want 2m", store.lastSave.AssumptionCheckInterval)
	}
}

// TestServer_NodeConfig_NewFieldsRoundTrip is a broad smoke test that
// the ADR-0070 field expansion is actually wired end to end (RPC
// request -> nodeconfig.Config -> RPC response), not just present in
// the proto.
func TestServer_NodeConfig_NewFieldsRoundTrip(t *testing.T) {
	store := &fakeNodeConfigStore{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)

	req := &rpcpb.UpdateNodeConfigRequest{
		ZfsBase:                         "zroot/apiary",
		BhyvePrefix:                     "apiary-",
		IsoDir:                          "/var/db/apiary/isos",
		JailPrefix:                      "apiary-",
		JailMountBase:                   "/apiary-jails",
		BhyveBootrom:                    "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd",
		BhyveBridge:                     "bridge0",
		DiskSizeMb:                      8192,
		JailDiskSizeMb:                  2048,
		HastEnabled:                     boolPtr(true),
		JailConsoleEnabled:              boolPtr(false),
		PeerTls:                         boolPtr(true),
		PeerManagerdPort:                "17700",
		PeerTlsHostnameMap:              "10.50.0.9=apiverse.apiary.work",
		TlsCert:                         "/home/claude/apiary-tls/fullchain.pem",
		TlsKey:                          "/home/claude/apiary-tls/key.pem",
		CloudflareTokenFile:             "/home/claude/cf-token",
		CloudflareZoneId:                "zone123",
		CloudflareTunnelId:              "tunnel456",
		CloudflareTunnelCredentialsFile: "/home/claude/cf-creds.json",
	}
	if _, err := s.UpdateNodeConfig(context.Background(), req); err != nil {
		t.Fatalf("UpdateNodeConfig() error: %v", err)
	}

	// Reflect the saved config back through Load/GetNodeConfig, the way
	// a real store would (the fake doesn't do this automatically).
	store.cfg = store.lastSave
	resp, err := s.GetNodeConfig(context.Background(), &rpcpb.GetNodeConfigRequest{})
	if err != nil {
		t.Fatalf("GetNodeConfig() error: %v", err)
	}

	checks := map[string]bool{
		"zfs_base":          resp.GetZfsBase() == "zroot/apiary",
		"bhyve_prefix":      resp.GetBhyvePrefix() == "apiary-",
		"iso_dir":           resp.GetIsoDir() == "/var/db/apiary/isos",
		"jail_prefix":       resp.GetJailPrefix() == "apiary-",
		"jail_mount_base":   resp.GetJailMountBase() == "/apiary-jails",
		"bhyve_bootrom":     resp.GetBhyveBootrom() != "",
		"bhyve_bridge":      resp.GetBhyveBridge() == "bridge0",
		"disk_size_mb":      resp.GetDiskSizeMb() == 8192,
		"jail_disk_size_mb": resp.GetJailDiskSizeMb() == 2048,
		"hast_enabled":      resp.GetHastEnabled(),
		"jail_console_enabled_unset": func() bool {
			return resp.JailConsoleEnabled != nil && !resp.GetJailConsoleEnabled()
		}(),
		"peer_tls":              resp.GetPeerTls(),
		"peer_managerd_port":    resp.GetPeerManagerdPort() == "17700",
		"peer_tls_hostname_map": resp.GetPeerTlsHostnameMap() == "10.50.0.9=apiverse.apiary.work",
		"tls_cert":              resp.GetTlsCert() != "",
		"tls_key":               resp.GetTlsKey() != "",
		"cloudflare_token_file": resp.GetCloudflareTokenFile() != "",
		"cloudflare_zone_id":    resp.GetCloudflareZoneId() == "zone123",
		"cloudflare_tunnel_id":  resp.GetCloudflareTunnelId() == "tunnel456",
		"cloudflare_creds_file": resp.GetCloudflareTunnelCredentialsFile() != "",
	}
	for field, ok := range checks {
		if !ok {
			t.Errorf("field %s did not round-trip correctly, got response: %+v", field, resp)
		}
	}
}

func boolPtr(v bool) *bool {
	return &v
}

// fakeQuotaSetter is a fake quotaSetter, without any real zfs(8) binary
// involved.
type fakeQuotaSetter struct {
	err          error
	lastName     string
	lastProperty string
	lastValue    string

	datasets       []string
	usedProperty   map[string]string
	destroyErr     error
	lastDestroyed  string
	existsOverride map[string]bool
}

func (f *fakeQuotaSetter) SetProperty(_ context.Context, name, prop, value string) error {
	if f.err != nil {
		return f.err
	}
	f.lastName, f.lastProperty, f.lastValue = name, prop, value
	return nil
}

func (f *fakeQuotaSetter) ListDatasets(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.datasets, nil
}

func (f *fakeQuotaSetter) GetProperty(_ context.Context, name, _ string) (string, error) {
	return f.usedProperty[name], nil
}

func (f *fakeQuotaSetter) DatasetExists(_ context.Context, name string) (bool, error) {
	if v, ok := f.existsOverride[name]; ok {
		return v, nil
	}
	for _, d := range f.datasets {
		if d == name {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeQuotaSetter) DestroyDataset(_ context.Context, name string) error {
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.lastDestroyed = name
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

func TestServer_ListOrphanedHASTResources_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.ListOrphanedHASTResources(context.Background(), &rpcpb.ListOrphanedHASTResourcesRequest{})
	if err != nil {
		t.Fatalf("ListOrphanedHASTResources() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("ListOrphanedHASTResources() error field = empty, want a not-configured message")
	}
}

func TestServer_CleanupOrphanedHASTResource_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.CleanupOrphanedHASTResource(context.Background(), &rpcpb.CleanupOrphanedHASTResourceRequest{ResourceId: "x", ResourceType: "vm"})
	if err != nil {
		t.Fatalf("CleanupOrphanedHASTResource() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("CleanupOrphanedHASTResource() error field = empty, want a not-configured message")
	}
}

func TestServer_CleanupOrphanedHASTResource_RequiresResourceID(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	resp, err := s.CleanupOrphanedHASTResource(context.Background(), &rpcpb.CleanupOrphanedHASTResourceRequest{ResourceType: "vm"})
	if err != nil {
		t.Fatalf("CleanupOrphanedHASTResource() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("CleanupOrphanedHASTResource() error field = empty, want a rejection for a missing resource_id")
	}
}

// TestServer_CleanupOrphanedHASTResource_RejectsInvalidResourceType checks
// this before ever touching raft (s.raft is nil here) - a plain
// validation failure must never depend on a real raft connection.
func TestServer_CleanupOrphanedHASTResource_RejectsInvalidResourceType(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	resp, err := s.CleanupOrphanedHASTResource(context.Background(), &rpcpb.CleanupOrphanedHASTResourceRequest{ResourceId: "x", ResourceType: "network"})
	if err != nil {
		t.Fatalf("CleanupOrphanedHASTResource() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("CleanupOrphanedHASTResource() error field = empty, want a rejection for an invalid resource_type")
	}
	if zfsMgr.lastDestroyed != "" {
		t.Errorf("DestroyDataset called (%q) for an invalid resource_type, want no destructive call at all", zfsMgr.lastDestroyed)
	}
}

// TestServer_CleanupOrphanedHASTResource_RejectsPathSeparatorInResourceID
// is the regression test for a security-audit finding: resource_id was
// concatenated directly into a ZFS dataset name with no validation of its
// own shape, relying entirely on zfs.Manager.path()'s generic per-segment
// traversal guard further downstream. A "/" must now be rejected at the
// point resource_id is first used to build that name, before ever reaching
// raft or zfs - checked here with a nil raft client to prove the rejection
// doesn't depend on either being reachable.
func TestServer_CleanupOrphanedHASTResource_RejectsPathSeparatorInResourceID(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{datasets: []string{"hast-vm-real"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	for _, id := range []string{"../real", "foo/../real", "foo/bar"} {
		t.Run(id, func(t *testing.T) {
			resp, err := s.CleanupOrphanedHASTResource(context.Background(), &rpcpb.CleanupOrphanedHASTResourceRequest{ResourceId: id, ResourceType: "vm"})
			if err != nil {
				t.Fatalf("CleanupOrphanedHASTResource() error: %v", err)
			}
			if resp.GetError() == "" {
				t.Fatalf("CleanupOrphanedHASTResource(%q) error field = empty, want a rejection for a resource_id containing a path separator", id)
			}
			if zfsMgr.lastDestroyed != "" {
				t.Fatalf("DestroyDataset called (%q) for resource_id %q, want no destructive call at all", zfsMgr.lastDestroyed, id)
			}
		})
	}
}

func TestServer_ListNodeServices_UsesLocalController(t *testing.T) {
	controller := &fakeNodeServiceController{services: []*rpcpb.NodeService{{Name: "apiary_frontend", Status: "running", Restartable: true}}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.services = controller

	resp, err := s.ListNodeServices(context.Background(), &rpcpb.ListNodeServicesRequest{})
	if err != nil {
		t.Fatalf("ListNodeServices() error: %v", err)
	}
	if got := resp.GetServices(); len(got) != 1 || got[0].GetName() != "apiary_frontend" {
		t.Errorf("services = %+v, want apiary_frontend", got)
	}
}

func TestServer_RestartNodeService_RejectsNonRestartableService(t *testing.T) {
	controller := &fakeNodeServiceController{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.services = controller

	resp, err := s.RestartNodeService(context.Background(), &rpcpb.RestartNodeServiceRequest{Name: "apiary_raftd"})
	if err != nil {
		t.Fatalf("RestartNodeService() error: %v", err)
	}
	if resp.GetError() == "" || controller.restartName != "" {
		t.Errorf("response = %+v; restart called for %q, want rejection without restart", resp, controller.restartName)
	}
}
