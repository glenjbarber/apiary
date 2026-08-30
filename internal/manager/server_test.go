package manager

import (
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hoststats"
	"github.com/glenjbarber/apiary/internal/isostore"
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

func TestServer_UploadISO_StreamsChunksIntoStore(t *testing.T) {
	isos := &fakeISOManager{}
	s := NewServer(nil, "node-1", isos, nil, nil)

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
	s := NewServer(nil, "node-1", &fakeISOManager{}, nil, nil)
	stream := &fakeUploadStream{reqs: []*rpcpb.UploadISORequest{chunkMsg("oops")}}

	if err := s.UploadISO(stream); err == nil {
		t.Fatalf("UploadISO() = nil error, want rejection when metadata isn't first")
	}
}

func TestServer_UploadISO_SaveErrorReportedInResponse(t *testing.T) {
	isos := &fakeISOManager{saveErr: errors.New("sha256 mismatch")}
	s := NewServer(nil, "node-1", isos, nil, nil)
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
	s := NewServer(nil, "node-1", isos, nil, nil)

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
	s := NewServer(nil, "node-1", isos, nil, nil)

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
	s := NewServer(nil, "node-1", isos, nil, nil)

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
	s := NewServer(nil, "node-1", &fakeISOManager{}, nil, nil)
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
