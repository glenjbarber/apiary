package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/isostore"
)

// fakeISOPeerForwarder embeds the (nil) PeerForwarder interface so it
// satisfies the full interface without stubbing every method by hand -
// only the ISO-replication methods actually exercised here are
// overridden; calling anything else would panic on the nil embedded
// interface, which is fine since no test here should ever reach them.
type fakeISOPeerForwarder struct {
	PeerForwarder

	mu sync.Mutex

	uploadCalls []struct{ addr, name, sha256 string }
	uploadErr   error
}

func (f *fakeISOPeerForwarder) UploadISO(_ context.Context, addr, name, expectedSHA256 string, _ io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadCalls = append(f.uploadCalls, struct{ addr, name, sha256 string }{addr, name, expectedSHA256})
	return f.uploadErr
}

// sha256Hex returns the hex-encoded SHA-256 of s, for building a valid
// isostore.Save call in tests without hard-coding a hash by hand.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newManagerdRPCClientWithISOAndPeers is newManagerdRPCClientFull, but
// lets a test supply isos/peers explicitly - needed for PushISOTo,
// which neither of the existing helper variants exercise.
func newManagerdRPCClientWithISOAndPeers(t *testing.T, raftdSocket, nodeID string, isos isoManager, peers PeerForwarder) rpcpb.ManagerServiceClient {
	t.Helper()

	raftClient, err := Dial(raftdSocket, "")
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { raftClient.Close() })

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(tcp) error: %v", err)
	}

	srv := NewServer(raftClient, nodeID, isos, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(srv.AuthUnaryInterceptor),
		grpc.StreamInterceptor(srv.AuthStreamInterceptor),
	)
	rpcpb.RegisterManagerServiceServer(grpcServer, srv)
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.GracefulStop)

	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return rpcpb.NewManagerServiceClient(conn)
}

func TestIntegration_PushISOTo_MissingFieldsIsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClientWithISOAndPeers(t, raftdSocket, "manager-1", isostore.New(t.TempDir()), &fakeISOPeerForwarder{})

	resp, err := client.PushISOTo(context.Background(), &rpcpb.PushISOToRequest{})
	if err != nil {
		t.Fatalf("PushISOTo() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("PushISOTo() with no name/target_node_id = no error, want a validation error")
	}
}

func TestIntegration_PushISOTo_NotPresentLocallyIsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClientWithISOAndPeers(t, raftdSocket, "manager-1", isostore.New(t.TempDir()), &fakeISOPeerForwarder{})

	resp, err := client.PushISOTo(context.Background(), &rpcpb.PushISOToRequest{Name: "missing.iso", TargetNodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("PushISOTo() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("PushISOTo() for a file not present locally = no error, want a clear rejection")
	}
}

func TestIntegration_PushISOTo_PushesPresentFileToResolvedTarget(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	peers := &fakeISOPeerForwarder{}
	isos := isostore.New(t.TempDir())
	if _, err := isos.Save("test.iso", strings.NewReader("hello"), sha256Hex("hello")); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	client := newManagerdRPCClientWithISOAndPeers(t, raftdSocket, "manager-1", isos, peers)

	resp, err := client.PushISOTo(context.Background(), &rpcpb.PushISOToRequest{Name: "test.iso", TargetNodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("PushISOTo() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("PushISOTo() error = %q, want success", resp.GetError())
	}

	peers.mu.Lock()
	defer peers.mu.Unlock()
	if len(peers.uploadCalls) != 1 || peers.uploadCalls[0].name != "test.iso" {
		t.Errorf("UploadISO calls = %+v, want one call for test.iso", peers.uploadCalls)
	}
}
