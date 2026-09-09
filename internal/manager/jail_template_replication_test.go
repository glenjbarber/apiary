package manager

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// fakeJailTemplatePeerForwarder embeds the (nil) PeerForwarder interface
// so it satisfies the full interface without stubbing every method by
// hand - the same pattern fakeISOPeerForwarder already establishes,
// just for PushJailTemplate (ADR-0089).
type fakeJailTemplatePeerForwarder struct {
	PeerForwarder

	mu sync.Mutex

	pushCalls []struct{ addr, name string }
	pushErr   error
}

func (f *fakeJailTemplatePeerForwarder) PushJailTemplate(_ context.Context, addr, name string, r io.Reader) error {
	io.Copy(io.Discard, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushCalls = append(f.pushCalls, struct{ addr, name string }{addr, name})
	return f.pushErr
}

func newManagerdRPCClientWithZFSAndPeers(t *testing.T, raftdSocket, nodeID string, zfsMgr quotaSetter, peers PeerForwarder) rpcpb.ManagerServiceClient {
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

	srv := NewServer(raftClient, nodeID, nil, nil, nil, nil, peers, "", zfsMgr, nil, nil, 0, nil)
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

// TestIntegration_PushJailTemplateTo_MissingFieldsIsError mirrors
// TestIntegration_PushISOTo_MissingFieldsIsError exactly, for the jail
// base-template equivalent RPC (ADR-0089).
func TestIntegration_PushJailTemplateTo_MissingFieldsIsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClientWithZFSAndPeers(t, raftdSocket, "manager-1", &fakeQuotaSetter{}, &fakeJailTemplatePeerForwarder{})

	resp, err := client.PushJailTemplateTo(context.Background(), &rpcpb.PushJailTemplateToRequest{})
	if err != nil {
		t.Fatalf("PushJailTemplateTo() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("PushJailTemplateTo() with no name/target_node_id = no error, want a validation error")
	}
}

// TestIntegration_PushJailTemplateTo_NoZFSConfiguredIsError confirms the
// nil-zfs guard: this RPC needs zfs.Send, unlike PushISOTo which only
// needs the isostore.
func TestIntegration_PushJailTemplateTo_NoZFSConfiguredIsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClientWithZFSAndPeers(t, raftdSocket, "manager-1", nil, &fakeJailTemplatePeerForwarder{})

	resp, err := client.PushJailTemplateTo(context.Background(), &rpcpb.PushJailTemplateToRequest{Name: "freebsd-14", TargetNodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("PushJailTemplateTo() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("PushJailTemplateTo() with no ZFS configured = no error, want a clear rejection")
	}
}

// TestIntegration_PushJailTemplateTo_PushesTemplateToResolvedTarget
// mirrors TestIntegration_PushISOTo_PushesPresentFileToResolvedTarget,
// confirming a local zfs.Send is streamed to the resolved target's
// PushJailTemplate.
func TestIntegration_PushJailTemplateTo_PushesTemplateToResolvedTarget(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	peers := &fakeJailTemplatePeerForwarder{}
	zfsMgr := &fakeQuotaSetter{sendData: map[string]string{"templates/freebsd-14@apiary-template": "root fs bytes"}}
	client := newManagerdRPCClientWithZFSAndPeers(t, raftdSocket, "manager-1", zfsMgr, peers)

	resp, err := client.PushJailTemplateTo(context.Background(), &rpcpb.PushJailTemplateToRequest{Name: "freebsd-14", TargetNodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("PushJailTemplateTo() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("PushJailTemplateTo() error = %q, want success", resp.GetError())
	}

	peers.mu.Lock()
	defer peers.mu.Unlock()
	if len(peers.pushCalls) != 1 || peers.pushCalls[0].name != "freebsd-14" {
		t.Errorf("PushJailTemplate calls = %+v, want one call for freebsd-14", peers.pushCalls)
	}
}

// TestIntegration_PushJailTemplateTo_SendErrorIsError confirms a local
// zfs.Send failure (e.g. the named template doesn't actually exist
// locally) is surfaced as a response error, not a panic or a push
// attempt.
func TestIntegration_PushJailTemplateTo_SendErrorIsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	peers := &fakeJailTemplatePeerForwarder{}
	zfsMgr := &fakeQuotaSetter{sendErr: errors.New("dataset does not exist")}
	client := newManagerdRPCClientWithZFSAndPeers(t, raftdSocket, "manager-1", zfsMgr, peers)

	resp, err := client.PushJailTemplateTo(context.Background(), &rpcpb.PushJailTemplateToRequest{Name: "missing", TargetNodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("PushJailTemplateTo() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("PushJailTemplateTo() with a Send error = no error, want a clear rejection")
	}
	peers.mu.Lock()
	defer peers.mu.Unlock()
	if len(peers.pushCalls) != 0 {
		t.Errorf("PushJailTemplate calls = %+v, want none - Send failed before any push was attempted", peers.pushCalls)
	}
}
