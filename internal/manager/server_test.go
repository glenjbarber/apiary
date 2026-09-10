package manager

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
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
	"github.com/glenjbarber/apiary/internal/origincert"
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

// fakeReceiveJailTemplateStream mirrors fakeUploadStream exactly, for
// Server.ReceiveJailTemplate (ADR-0089).
type fakeReceiveJailTemplateStream struct {
	grpc.ServerStream
	reqs []*rpcpb.ReceiveJailTemplateRequest
	idx  int
	resp *rpcpb.ReceiveJailTemplateResponse
}

func (f *fakeReceiveJailTemplateStream) Recv() (*rpcpb.ReceiveJailTemplateRequest, error) {
	if f.idx >= len(f.reqs) {
		return nil, io.EOF
	}
	req := f.reqs[f.idx]
	f.idx++
	return req, nil
}

func (f *fakeReceiveJailTemplateStream) SendAndClose(resp *rpcpb.ReceiveJailTemplateResponse) error {
	f.resp = resp
	return nil
}

func (f *fakeReceiveJailTemplateStream) Context() context.Context { return context.Background() }

func templateMetadataMsg(name string) *rpcpb.ReceiveJailTemplateRequest {
	return &rpcpb.ReceiveJailTemplateRequest{Data: &rpcpb.ReceiveJailTemplateRequest_Metadata{
		Metadata: &rpcpb.JailTemplateMetadata{Name: name},
	}}
}

func templateChunkMsg(data string) *rpcpb.ReceiveJailTemplateRequest {
	return &rpcpb.ReceiveJailTemplateRequest{Data: &rpcpb.ReceiveJailTemplateRequest_Chunk{Chunk: []byte(data)}}
}

func TestServer_ReceiveJailTemplate_StreamsChunksIntoZFS(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	stream := &fakeReceiveJailTemplateStream{reqs: []*rpcpb.ReceiveJailTemplateRequest{
		templateMetadataMsg("freebsd-14"),
		templateChunkMsg("hello "),
		templateChunkMsg("world"),
	}}

	if err := s.ReceiveJailTemplate(stream); err != nil {
		t.Fatalf("ReceiveJailTemplate() error: %v", err)
	}
	if zfsMgr.receivedInto["templates/freebsd-14"] != "hello world" {
		t.Errorf("received data = %q, want %q", zfsMgr.receivedInto["templates/freebsd-14"], "hello world")
	}
	if stream.resp.GetError() != "" {
		t.Errorf("response error = %q, want empty", stream.resp.GetError())
	}
	if stream.resp.GetName() != "freebsd-14" {
		t.Errorf("response name = %q, want freebsd-14", stream.resp.GetName())
	}
}

func TestServer_ReceiveJailTemplate_MissingMetadataFirstIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", &fakeQuotaSetter{}, nil, nil, 0, nil)
	stream := &fakeReceiveJailTemplateStream{reqs: []*rpcpb.ReceiveJailTemplateRequest{templateChunkMsg("oops")}}

	if err := s.ReceiveJailTemplate(stream); err == nil {
		t.Fatal("ReceiveJailTemplate() = nil error, want rejection when metadata isn't first")
	}
}

func TestServer_ReceiveJailTemplate_NoZFSConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	stream := &fakeReceiveJailTemplateStream{reqs: []*rpcpb.ReceiveJailTemplateRequest{templateMetadataMsg("freebsd-14")}}

	if err := s.ReceiveJailTemplate(stream); err != nil {
		t.Fatalf("ReceiveJailTemplate() error: %v, want a response-level error instead", err)
	}
	if stream.resp.GetError() == "" {
		t.Error("response error = empty, want the no-ZFS-configured error surfaced")
	}
}

func TestServer_ReceiveJailTemplate_ReceiveErrorReportedInResponse(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{receiveErr: errors.New("zfs receive failed: stream corrupt")}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)
	stream := &fakeReceiveJailTemplateStream{reqs: []*rpcpb.ReceiveJailTemplateRequest{
		templateMetadataMsg("freebsd-14"),
		templateChunkMsg("data"),
	}}

	if err := s.ReceiveJailTemplate(stream); err != nil {
		t.Fatalf("ReceiveJailTemplate() error: %v, want a response-level error instead", err)
	}
	if stream.resp.GetError() == "" {
		t.Error("response error = empty, want the receive error surfaced")
	}
}

func TestServer_ListJailTemplateNames(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{templateNames: []string{"freebsd-14", "debian-12"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	resp, err := s.ListJailTemplateNames(context.Background(), &rpcpb.ListJailTemplateNamesRequest{})
	if err != nil {
		t.Fatalf("ListJailTemplateNames() error: %v", err)
	}
	if len(resp.GetNames()) != 2 || resp.GetNames()[0] != "freebsd-14" {
		t.Errorf("ListJailTemplateNames() = %+v, want [freebsd-14 debian-12]", resp.GetNames())
	}
}

func TestServer_ListJailTemplateNames_ErrorSurfacedInResponse(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{templateNamesErr: errors.New("zfs list failed")}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	resp, err := s.ListJailTemplateNames(context.Background(), &rpcpb.ListJailTemplateNamesRequest{})
	if err != nil {
		t.Fatalf("ListJailTemplateNames() error: %v, want a response-level error instead", err)
	}
	if resp.GetError() == "" {
		t.Error("response error = empty, want the underlying error surfaced")
	}
}

func TestServer_ListJailTemplateNames_NotConfiguredReturnsEmpty(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.ListJailTemplateNames(context.Background(), &rpcpb.ListJailTemplateNamesRequest{})
	if err != nil {
		t.Fatalf("ListJailTemplateNames() error: %v", err)
	}
	if len(resp.GetNames()) != 0 || resp.GetError() != "" {
		t.Errorf("ListJailTemplateNames() = %+v, want an empty, error-free response when no ZFS is configured", resp)
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

	artifactPresent     bool
	artifactBridge      string
	artifactOwnBridge   bool
	artifactOwnVLAN     bool
	artifactOutboundNAT bool
	artifactErr         error
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
func (f *fakeReconcilerStats) NetworkArtifactStatus(string) (bool, string, bool, bool, bool, error) {
	return f.artifactPresent, f.artifactBridge, f.artifactOwnBridge, f.artifactOwnVLAN, f.artifactOutboundNAT, f.artifactErr
}

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

type fakeOriginCAIssuer struct {
	token        string
	hostnames    []string
	validityDays int
}

func (f *fakeOriginCAIssuer) Issue(_ context.Context, token string, hostnames []string, validityDays int, csrPEM string) (origincert.IssuedCertificate, error) {
	f.token = token
	f.hostnames = append([]string(nil), hostnames...)
	f.validityDays = validityDays
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		return origincert.IssuedCertificate{}, errors.New("invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return origincert.IssuedCertificate{}, err
	}
	if err := csr.CheckSignature(); err != nil {
		return origincert.IssuedCertificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return origincert.IssuedCertificate{}, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		DNSNames:     csr.DNSNames,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, csr.PublicKey, key)
	if err != nil {
		return origincert.IssuedCertificate{}, err
	}
	return origincert.IssuedCertificate{ID: "origin-cert-1", ExpiresAt: tmpl.NotAfter,
		PEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, nil
}

func (f *fakeNodeServiceController) List(context.Context) ([]*rpcpb.NodeService, error) {
	return f.services, f.listErr
}

func (f *fakeNodeServiceController) Restart(_ context.Context, name string) error {
	f.restartName = name
	return f.restartErr
}

func TestServer_IssueOriginCertificateInstallsAndRestartsManagerd(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "origin-ca.token")
	if err := os.WriteFile(tokenPath, []byte("token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fakeNodeConfigStore{cfg: nodeconfig.Config{
		OriginCATokenFile: tokenPath,
		OriginCADirectory: dir,
		TLSCert:           filepath.Join(dir, "managerd.crt"),
		TLSKey:            filepath.Join(dir, "managerd.key"),
	}}
	issuer := &fakeOriginCAIssuer{}
	services := &fakeNodeServiceController{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
	s.SetOriginCAIssuer(issuer)
	s.services = services

	resp, err := s.IssueOriginCertificate(context.Background(), &rpcpb.IssueOriginCertificateRequest{
		Name: "managerd", Hostnames: []string{"apiary.example.com"}, ValidityDays: 365,
	})
	if err != nil {
		t.Fatalf("IssueOriginCertificate() error: %v", err)
	}
	if resp.GetError() != "" || !resp.GetRestartScheduled() {
		t.Fatalf("IssueOriginCertificate() = %+v, want success and restart", resp)
	}
	if issuer.token != "token-value" || issuer.validityDays != 365 {
		t.Fatalf("issuer received token=%q validity=%d", issuer.token, issuer.validityDays)
	}
	if got := issuer.hostnames; len(got) != 1 || got[0] != "apiary.example.com" {
		t.Fatalf("issuer hostnames = %q", got)
	}
	if services.restartName != "apiary_managerd" {
		t.Fatalf("restart service = %q, want apiary_managerd", services.restartName)
	}
	if _, err := os.Stat(filepath.Join(dir, "managerd.crt")); err != nil {
		t.Fatalf("certificate was not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "managerd.key")); err != nil {
		t.Fatalf("private key was not installed: %v", err)
	}
}

func TestServer_IssueOriginCertificateAutoRenewRoundTripsThroughListOriginCertificates(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "origin-ca.token")
	if err := os.WriteFile(tokenPath, []byte("token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fakeNodeConfigStore{cfg: nodeconfig.Config{
		OriginCATokenFile: tokenPath,
		OriginCADirectory: dir,
		TLSCert:           filepath.Join(dir, "managerd.crt"),
		TLSKey:            filepath.Join(dir, "managerd.key"),
	}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
	s.SetOriginCAIssuer(&fakeOriginCAIssuer{})
	s.services = &fakeNodeServiceController{}

	resp, err := s.IssueOriginCertificate(context.Background(), &rpcpb.IssueOriginCertificateRequest{
		Name: "managerd", Hostnames: []string{"apiary.example.com"}, ValidityDays: 365, AutoRenew: true,
	})
	if err != nil {
		t.Fatalf("IssueOriginCertificate() error: %v", err)
	}
	if resp.GetError() != "" || !resp.GetCertificate().GetAutoRenew() {
		t.Fatalf("IssueOriginCertificate() = %+v, want AutoRenew true on the returned certificate", resp)
	}

	listResp, err := s.ListOriginCertificates(context.Background(), &rpcpb.ListOriginCertificatesRequest{})
	if err != nil {
		t.Fatalf("ListOriginCertificates() error: %v", err)
	}
	if len(listResp.GetCertificates()) != 1 || !listResp.GetCertificates()[0].GetAutoRenew() {
		t.Fatalf("ListOriginCertificates() = %+v, want one certificate with AutoRenew true", listResp.GetCertificates())
	}
}

func TestServer_IssueOriginCertificateDefaultsAutoRenewFalse(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "origin-ca.token")
	if err := os.WriteFile(tokenPath, []byte("token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fakeNodeConfigStore{cfg: nodeconfig.Config{
		OriginCATokenFile: tokenPath,
		OriginCADirectory: dir,
		TLSCert:           filepath.Join(dir, "managerd.crt"),
		TLSKey:            filepath.Join(dir, "managerd.key"),
	}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
	s.SetOriginCAIssuer(&fakeOriginCAIssuer{})
	s.services = &fakeNodeServiceController{}

	resp, err := s.IssueOriginCertificate(context.Background(), &rpcpb.IssueOriginCertificateRequest{
		Name: "managerd", Hostnames: []string{"apiary.example.com"}, ValidityDays: 365,
	})
	if err != nil {
		t.Fatalf("IssueOriginCertificate() error: %v", err)
	}
	if resp.GetCertificate().GetAutoRenew() {
		t.Fatalf("IssueOriginCertificate() = %+v, want AutoRenew false when not requested", resp.GetCertificate())
	}
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
		"zfs_base":              resp.GetZfsBase() == "zroot/apiary",
		"bhyve_prefix":          resp.GetBhyvePrefix() == "apiary-",
		"iso_dir":               resp.GetIsoDir() == "/var/db/apiary/isos",
		"jail_prefix":           resp.GetJailPrefix() == "apiary-",
		"jail_mount_base":       resp.GetJailMountBase() == "/apiary-jails",
		"bhyve_bootrom":         resp.GetBhyveBootrom() != "",
		"bhyve_bridge":          resp.GetBhyveBridge() == "bridge0",
		"disk_size_mb":          resp.GetDiskSizeMb() == 8192,
		"jail_disk_size_mb":     resp.GetJailDiskSizeMb() == 2048,
		"hast_enabled":          resp.GetHastEnabled(),
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

	// sendData/sendErr, receivedInto/receiveErr, templateNames/
	// templateNamesErr back Send/Receive/ListTemplateNames' tests
	// (ADR-0089) - the jail base-template peer-fetch primitives.
	sendData map[string]string
	sendErr  error

	receivedInto map[string]string // destName -> received bytes
	receiveErr   error

	templateNames    []string
	templateNamesErr error

	// snapshots/createSnapshotErr/rollbackSnapshotErr/destroySnapshotErr/
	// listSnapshotsErr back CreateSnapshot/RollbackSnapshot/
	// DestroySnapshot/ListSnapshots' tests (ADR-0090). snapshots is
	// keyed by the full "dataset@snapshot" name passed in, mirroring how
	// the real Manager scopes a snapshot to one specific dataset.
	snapshots           map[string]bool
	createSnapshotErr   error
	rollbackSnapshotErr error
	destroySnapshotErr  error
	listSnapshotsErr    error
	lastRolledBack      string
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

func (f *fakeQuotaSetter) Send(_ context.Context, snapshot string) (io.ReadCloser, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	data, ok := f.sendData[snapshot]
	if !ok {
		return nil, fmt.Errorf("dataset does not exist: %q", snapshot)
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

func (f *fakeQuotaSetter) Receive(_ context.Context, destName string, r io.Reader) error {
	if f.receiveErr != nil {
		return f.receiveErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if f.receivedInto == nil {
		f.receivedInto = map[string]string{}
	}
	f.receivedInto[destName] = string(data)
	return nil
}

func (f *fakeQuotaSetter) ListTemplateNames(context.Context) ([]string, error) {
	if f.templateNamesErr != nil {
		return nil, f.templateNamesErr
	}
	return f.templateNames, nil
}

func (f *fakeQuotaSetter) CreateSnapshot(_ context.Context, name string) error {
	if f.createSnapshotErr != nil {
		return f.createSnapshotErr
	}
	if f.snapshots == nil {
		f.snapshots = map[string]bool{}
	}
	f.snapshots[name] = true
	return nil
}

func (f *fakeQuotaSetter) RollbackSnapshot(_ context.Context, name string) error {
	if f.rollbackSnapshotErr != nil {
		return f.rollbackSnapshotErr
	}
	f.lastRolledBack = name
	return nil
}

func (f *fakeQuotaSetter) DestroySnapshot(_ context.Context, name string) error {
	if f.destroySnapshotErr != nil {
		return f.destroySnapshotErr
	}
	delete(f.snapshots, name)
	return nil
}

func (f *fakeQuotaSetter) ListSnapshots(_ context.Context, datasetName string) ([]string, error) {
	if f.listSnapshotsErr != nil {
		return nil, f.listSnapshotsErr
	}
	prefix := datasetName + "@"
	var names []string
	for full := range f.snapshots {
		if strings.HasPrefix(full, prefix) {
			names = append(names, strings.TrimPrefix(full, prefix))
		}
	}
	return names, nil
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

func TestServer_CreateVMSnapshot(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	resp, err := s.CreateVMSnapshot(context.Background(), &rpcpb.CreateVMSnapshotRequest{Id: "vm-1", SnapshotName: "before-migration"})
	if err != nil {
		t.Fatalf("CreateVMSnapshot() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("CreateVMSnapshot() returned error: %s", resp.GetError())
	}
	if !zfsMgr.snapshots["vm-1@before-migration"] {
		t.Errorf("snapshots = %v, want vm-1@before-migration present", zfsMgr.snapshots)
	}
}

func TestServer_CreateVMSnapshot_MissingSnapshotNameIsError(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	resp, err := s.CreateVMSnapshot(context.Background(), &rpcpb.CreateVMSnapshotRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("CreateVMSnapshot() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("CreateVMSnapshot() with no snapshot_name = no error, want a validation error")
	}
}

func TestServer_CreateVMSnapshot_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.CreateVMSnapshot(context.Background(), &rpcpb.CreateVMSnapshotRequest{Id: "vm-1", SnapshotName: "x"})
	if err != nil {
		t.Fatalf("CreateVMSnapshot() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("CreateVMSnapshot() with no ZFS configured = no error, want a clear rejection")
	}
}

func TestServer_ListVMSnapshots(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)
	zfsMgr.CreateSnapshot(context.Background(), "vm-1@first")
	zfsMgr.CreateSnapshot(context.Background(), "vm-1@second")
	zfsMgr.CreateSnapshot(context.Background(), "vm-2@unrelated")

	resp, err := s.ListVMSnapshots(context.Background(), &rpcpb.ListVMSnapshotsRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("ListVMSnapshots() error: %v", err)
	}
	if len(resp.GetSnapshotNames()) != 2 {
		t.Errorf("ListVMSnapshots() = %v, want exactly vm-1's own two snapshots, not vm-2's", resp.GetSnapshotNames())
	}
}

func TestServer_ListVMSnapshots_NotConfiguredReturnsEmpty(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.ListVMSnapshots(context.Background(), &rpcpb.ListVMSnapshotsRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("ListVMSnapshots() error: %v", err)
	}
	if len(resp.GetSnapshotNames()) != 0 || resp.GetError() != "" {
		t.Errorf("ListVMSnapshots() = %+v, want an empty, error-free response when no ZFS is configured", resp)
	}
}

// TestServer_RestoreVMSnapshot_NilRaftSkipsRunningCheck guards the real
// bug this test itself caught during development: RestoreVMSnapshot
// used to call s.GetVM unconditionally, which panics on a nil s.raft
// (every other quotaSetter-only unit test in this file constructs the
// Server that way). The running-VM check must be skippable, not just
// error-tolerant, when there's no raft client configured at all.
func TestServer_RestoreVMSnapshot_NilRaftSkipsRunningCheck(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	resp, err := s.RestoreVMSnapshot(context.Background(), &rpcpb.RestoreVMSnapshotRequest{Id: "vm-1", SnapshotName: "before-migration"})
	if err != nil {
		t.Fatalf("RestoreVMSnapshot() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RestoreVMSnapshot() returned error: %s", resp.GetError())
	}
	if zfsMgr.lastRolledBack != "vm-1@before-migration" {
		t.Errorf("RollbackSnapshot called with %q, want vm-1@before-migration", zfsMgr.lastRolledBack)
	}
}

func TestServer_RestoreVMSnapshot_ZFSErrorSurfacedInResponse(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{rollbackSnapshotErr: errors.New("snapshot does not exist")}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)

	resp, err := s.RestoreVMSnapshot(context.Background(), &rpcpb.RestoreVMSnapshotRequest{Id: "vm-1", SnapshotName: "no-such-snapshot"})
	if err != nil {
		t.Fatalf("RestoreVMSnapshot() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("RestoreVMSnapshot() with a rollback error = no error, want it surfaced")
	}
}

func TestServer_RestoreVMSnapshot_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.RestoreVMSnapshot(context.Background(), &rpcpb.RestoreVMSnapshotRequest{Id: "vm-1", SnapshotName: "x"})
	if err != nil {
		t.Fatalf("RestoreVMSnapshot() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("RestoreVMSnapshot() with no ZFS configured = no error, want a clear rejection")
	}
}

func TestServer_DeleteVMSnapshot(t *testing.T) {
	zfsMgr := &fakeQuotaSetter{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", zfsMgr, nil, nil, 0, nil)
	zfsMgr.CreateSnapshot(context.Background(), "vm-1@stale")

	resp, err := s.DeleteVMSnapshot(context.Background(), &rpcpb.DeleteVMSnapshotRequest{Id: "vm-1", SnapshotName: "stale"})
	if err != nil {
		t.Fatalf("DeleteVMSnapshot() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("DeleteVMSnapshot() returned error: %s", resp.GetError())
	}
	if zfsMgr.snapshots["vm-1@stale"] {
		t.Error("snapshot still present after DeleteVMSnapshot()")
	}
}

func TestServer_DeleteVMSnapshot_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.DeleteVMSnapshot(context.Background(), &rpcpb.DeleteVMSnapshotRequest{Id: "vm-1", SnapshotName: "x"})
	if err != nil {
		t.Fatalf("DeleteVMSnapshot() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("DeleteVMSnapshot() with no ZFS configured = no error, want a clear rejection")
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

func TestServer_GetNetworkTeardownStatus_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.GetNetworkTeardownStatus(context.Background(), &rpcpb.GetNetworkTeardownStatusRequest{NetworkId: "net-1"})
	if err != nil {
		t.Fatalf("GetNetworkTeardownStatus() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("GetNetworkTeardownStatus() error field = empty, want a not-configured message")
	}
}

func TestServer_GetNetworkTeardownStatus_ReportsPresentArtifact(t *testing.T) {
	reconciler := &fakeReconcilerStats{artifactPresent: true, artifactBridge: "bridge5", artifactOwnBridge: true, artifactOwnVLAN: true}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, reconciler)

	resp, err := s.GetNetworkTeardownStatus(context.Background(), &rpcpb.GetNetworkTeardownStatusRequest{NetworkId: "net-1"})
	if err != nil || resp.GetError() != "" {
		t.Fatalf("GetNetworkTeardownStatus() = (%+v, %v)", resp, err)
	}
	if !resp.GetPresent() || resp.GetBridge() != "bridge5" || !resp.GetOwnBridge() || !resp.GetOwnVlan() {
		t.Errorf("GetNetworkTeardownStatus() = %+v, want present artifact details", resp)
	}
}

func TestServer_GetNetworkTeardownStatus_ClearWhenAbsent(t *testing.T) {
	reconciler := &fakeReconcilerStats{artifactPresent: false}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, reconciler)

	resp, err := s.GetNetworkTeardownStatus(context.Background(), &rpcpb.GetNetworkTeardownStatusRequest{NetworkId: "net-1"})
	if err != nil || resp.GetError() != "" {
		t.Fatalf("GetNetworkTeardownStatus() = (%+v, %v)", resp, err)
	}
	if resp.GetPresent() {
		t.Errorf("GetNetworkTeardownStatus() Present = true, want false when no artifact recorded")
	}
}

func TestServer_GetNetworkTeardownStatus_ReadErrorSurfacesAsError(t *testing.T) {
	reconciler := &fakeReconcilerStats{artifactErr: errors.New("state file corrupt")}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, reconciler)

	resp, err := s.GetNetworkTeardownStatus(context.Background(), &rpcpb.GetNetworkTeardownStatusRequest{NetworkId: "net-1"})
	if err != nil {
		t.Fatalf("GetNetworkTeardownStatus() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("GetNetworkTeardownStatus() error field = empty, want the reconciler's own read error surfaced")
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

func TestServer_GetUplinkStatus_NoVLANConfigured(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.GetUplinkStatus(context.Background(), &rpcpb.GetUplinkStatusRequest{})
	if err != nil {
		t.Fatalf("GetUplinkStatus() error: %v", err)
	}
	if resp.GetConfigured() {
		t.Errorf("Configured = true, want false with no VLAN manager")
	}
}

func TestServer_GetUplinkStatus_ReportsInterfaceAndState(t *testing.T) {
	vlan := &fakeVLANStatus{uplink: "em0", up: map[string]bool{"em0": true}}
	s := NewServer(nil, "node-1", nil, nil, nil, vlan, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.GetUplinkStatus(context.Background(), &rpcpb.GetUplinkStatusRequest{})
	if err != nil {
		t.Fatalf("GetUplinkStatus() error: %v", err)
	}
	if !resp.GetConfigured() || resp.GetInterface() != "em0" || !resp.GetUp() {
		t.Errorf("response = %+v, want configured=true interface=em0 up=true", resp)
	}
}

func TestServer_SetUplinkState_NoVLANConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.SetUplinkState(context.Background(), &rpcpb.SetUplinkStateRequest{Down: true})
	if err != nil {
		t.Fatalf("SetUplinkState() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("response = %+v, want an error with no VLAN manager configured", resp)
	}
}

func TestServer_SetUplinkState_DownThenUpRoundTrips(t *testing.T) {
	vlan := &fakeVLANStatus{uplink: "em0", up: map[string]bool{"em0": true}}
	s := NewServer(nil, "node-1", nil, nil, nil, vlan, nil, "", nil, nil, nil, 0, nil)

	downResp, err := s.SetUplinkState(context.Background(), &rpcpb.SetUplinkStateRequest{Down: true})
	if err != nil {
		t.Fatalf("SetUplinkState(down) error: %v", err)
	}
	if downResp.GetError() != "" || downResp.GetUp() {
		t.Fatalf("SetUplinkState(down) = %+v, want up=false no error", downResp)
	}
	if up := vlan.up["em0"]; up {
		t.Errorf("fake interface state = up, want down after SetUplinkState(down)")
	}

	upResp, err := s.SetUplinkState(context.Background(), &rpcpb.SetUplinkStateRequest{Down: false})
	if err != nil {
		t.Fatalf("SetUplinkState(up) error: %v", err)
	}
	if upResp.GetError() != "" || !upResp.GetUp() {
		t.Fatalf("SetUplinkState(up) = %+v, want up=true no error", upResp)
	}
	if up := vlan.up["em0"]; !up {
		t.Errorf("fake interface state = down, want up after SetUplinkState(up)")
	}
}

// fakeNATPauser is a fake natPauser for SetUplinkState's NAT-pause
// side effect (ADR-0088), without any real pf(8) involved.
type fakeNATPauser struct {
	uplink  string
	flushed []string
	err     error
	calls   int
}

func (f *fakeNATPauser) NATUplink() string { return f.uplink }

func (f *fakeNATPauser) PauseOutboundNAT(context.Context) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.flushed, nil
}

func TestServer_SetUplinkState_PausesNATWhenUplinkMatches(t *testing.T) {
	vlan := &fakeVLANStatus{uplink: "em0", up: map[string]bool{"em0": true}}
	nat := &fakeNATPauser{uplink: "em0", flushed: []string{"net-1", "net-2"}}
	s := NewServer(nil, "node-1", nil, nil, nil, vlan, nil, "", nil, nil, nil, 0, nil)
	s.SetNATPauser(nat)

	resp, err := s.SetUplinkState(context.Background(), &rpcpb.SetUplinkStateRequest{Down: true})
	if err != nil {
		t.Fatalf("SetUplinkState(down) error: %v", err)
	}
	if nat.calls != 1 {
		t.Fatalf("PauseOutboundNAT calls = %d, want 1 when the toggled interface matches NATUplink", nat.calls)
	}
	if got := resp.GetNatPausedNetworks(); len(got) != 2 || got[0] != "net-1" || got[1] != "net-2" {
		t.Errorf("NatPausedNetworks = %v, want [net-1 net-2]", got)
	}
}

func TestServer_SetUplinkState_SkipsNATPauseWhenUplinksDiffer(t *testing.T) {
	// ADR-0048's own disclosed case: -vlan-uplink and -nat-uplink can
	// name different physical interfaces - downing one must not pause
	// NAT that depends on the other, unrelated one.
	vlan := &fakeVLANStatus{uplink: "em0", up: map[string]bool{"em0": true}}
	nat := &fakeNATPauser{uplink: "bridge0", flushed: []string{"net-1"}}
	s := NewServer(nil, "node-1", nil, nil, nil, vlan, nil, "", nil, nil, nil, 0, nil)
	s.SetNATPauser(nat)

	resp, err := s.SetUplinkState(context.Background(), &rpcpb.SetUplinkStateRequest{Down: true})
	if err != nil {
		t.Fatalf("SetUplinkState(down) error: %v", err)
	}
	if nat.calls != 0 {
		t.Errorf("PauseOutboundNAT calls = %d, want 0 when the toggled interface doesn't match NATUplink", nat.calls)
	}
	if got := resp.GetNatPausedNetworks(); len(got) != 0 {
		t.Errorf("NatPausedNetworks = %v, want none", got)
	}
}

func TestServer_SetUplinkState_SkipsNATPauseWhenNoPauserConfigured(t *testing.T) {
	vlan := &fakeVLANStatus{uplink: "em0", up: map[string]bool{"em0": true}}
	s := NewServer(nil, "node-1", nil, nil, nil, vlan, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.SetUplinkState(context.Background(), &rpcpb.SetUplinkStateRequest{Down: true})
	if err != nil {
		t.Fatalf("SetUplinkState(down) error: %v", err)
	}
	if resp.GetError() != "" {
		t.Errorf("response = %+v, want no error with no natPauser configured", resp)
	}
}

func TestServer_SetUplinkState_NATPauseFailureDoesNotFailTheRPC(t *testing.T) {
	vlan := &fakeVLANStatus{uplink: "em0", up: map[string]bool{"em0": true}}
	nat := &fakeNATPauser{uplink: "em0", err: errors.New("pfctl: anchor busy")}
	s := NewServer(nil, "node-1", nil, nil, nil, vlan, nil, "", nil, nil, nil, 0, nil)
	s.SetNATPauser(nat)

	resp, err := s.SetUplinkState(context.Background(), &rpcpb.SetUplinkStateRequest{Down: true})
	if err != nil {
		t.Fatalf("SetUplinkState(down) error: %v", err)
	}
	if resp.GetError() != "" || resp.GetUp() {
		t.Errorf("response = %+v, want the primary down action to still succeed despite the NAT-pause error", resp)
	}
}

func TestServer_SetUplinkState_PropagatesDownError(t *testing.T) {
	vlan := &fakeVLANStatus{uplink: "em0", downErr: errors.New("ifconfig em0 down: device busy")}
	s := NewServer(nil, "node-1", nil, nil, nil, vlan, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.SetUplinkState(context.Background(), &rpcpb.SetUplinkStateRequest{Down: true})
	if err != nil {
		t.Fatalf("SetUplinkState() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("response = %+v, want the underlying ifconfig error surfaced", resp)
	}
}

// fakePAMAuthenticator is a fake pam.Authenticator for
// AuthenticatePassword tests (ADR-0087), without any real libpam
// involved.
type fakePAMAuthenticator struct {
	user, pass string
	err        error
}

func (f fakePAMAuthenticator) Authenticate(username, password string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return username == f.user && password == f.pass, nil
}

func TestServer_AuthenticatePassword_NoPAMConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	resp, err := s.AuthenticatePassword(context.Background(), &rpcpb.AuthenticatePasswordRequest{Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatalf("AuthenticatePassword() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("response = %+v, want an error with no PAM authenticator configured", resp)
	}
}

func TestServer_AuthenticatePassword_WrapsRealAuthenticator(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetPAMAuthenticator(fakePAMAuthenticator{user: "alice", pass: "secret"})

	ok, err := s.AuthenticatePassword(context.Background(), &rpcpb.AuthenticatePasswordRequest{Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatalf("AuthenticatePassword() error: %v", err)
	}
	if !ok.GetOk() || ok.GetError() != "" {
		t.Errorf("response = %+v, want ok=true no error for correct credentials", ok)
	}

	bad, err := s.AuthenticatePassword(context.Background(), &rpcpb.AuthenticatePasswordRequest{Username: "alice", Password: "wrong"})
	if err != nil {
		t.Fatalf("AuthenticatePassword() error: %v", err)
	}
	if bad.GetOk() {
		t.Errorf("response = %+v, want ok=false for wrong password", bad)
	}
}

func TestServer_AuthenticatePassword_PropagatesBackendError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetPAMAuthenticator(fakePAMAuthenticator{err: errors.New("pam: starting transaction: System error")})

	resp, err := s.AuthenticatePassword(context.Background(), &rpcpb.AuthenticatePasswordRequest{Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatalf("AuthenticatePassword() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Errorf("response = %+v, want the backend error surfaced", resp)
	}
}
