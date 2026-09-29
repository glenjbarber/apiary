package manager

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// fakeVMSnapshotPeerForwarder embeds the (nil) PeerForwarder interface so
// it satisfies the full interface without stubbing every method by hand
// - the pattern fakeISOPeerForwarder and
// fakeJailTemplatePeerForwarder already establish, here for
// PushVMSnapshot (ADR-0090's cross-node follow-up).
type fakeVMSnapshotPeerForwarder struct {
	PeerForwarder

	mu sync.Mutex

	pushCalls []struct{ addr, vmID, snapshotName string }
	pushErr   error
}

func (f *fakeVMSnapshotPeerForwarder) PushVMSnapshot(_ context.Context, addr, vmID, snapshotName string, r io.Reader) error {
	io.Copy(io.Discard, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushCalls = append(f.pushCalls, struct{ addr, vmID, snapshotName string }{addr, vmID, snapshotName})
	return f.pushErr
}

// startManagerdWithZFS is newManagerdRPCClientWithZFSAndPeers, but it
// also returns the listener address, so a test can point a real
// *PeerReporter at this managerd - which is the only way to exercise the
// actual client stream rather than a stand-in for it.
func startManagerdWithZFS(t *testing.T, raftdSocket, nodeID string, zfsMgr quotaSetter, peers PeerForwarder) (rpcpb.ManagerServiceClient, string) {
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

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient() error: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return rpcpb.NewManagerServiceClient(conn), lis.Addr().String()
}

// seedVMDefinition puts a VM definition into a node's raft log, which
// is what a target needs before it will accept a VM snapshot for that
// VM. This is not a test-only convenience: every Comb in a Colony
// applies the same replicated log, so a VM an operator created through
// CreateVM is already defined on all of them. What stays node-local -
// and is the entire reason this transfer pair exists - is the
// DATASET, not the definition.
func seedVMDefinition(t *testing.T, client rpcpb.ManagerServiceClient, id, nodeID string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{Vm: &rpcpb.VMDefinition{Id: id, Name: id, NodeId: nodeID}})
	if err != nil {
		t.Fatalf("CreateVM(%q) error: %v", id, err)
	}
	if resp.GetError() != "" {
		t.Fatalf("CreateVM(%q) error = %q", id, resp.GetError())
	}
}

// seedVMWithState is seedVMDefinition for a VM an operator has asked
// to be running, which is the state the running-VM half of the
// receive preconditions is written against.
func seedVMWithState(t *testing.T, client rpcpb.ManagerServiceClient, id, nodeID string, state rpcpb.VMState) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{Vm: &rpcpb.VMDefinition{Id: id, Name: id, NodeId: nodeID, DesiredState: state}})
	if err != nil {
		t.Fatalf("CreateVM(%q) error: %v", id, err)
	}
	if resp.GetError() != "" {
		t.Fatalf("CreateVM(%q) error = %q", id, resp.GetError())
	}
}

func TestIntegration_PushVMSnapshotTo_MissingFieldsIsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClientWithZFSAndPeers(t, raftdSocket, "manager-1", &fakeQuotaSetter{}, &fakeVMSnapshotPeerForwarder{})

	resp, err := client.PushVMSnapshotTo(context.Background(), &rpcpb.PushVMSnapshotToRequest{})
	if err != nil {
		t.Fatalf("PushVMSnapshotTo() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("PushVMSnapshotTo() with no id/snapshot_name/target_node_id = no error, want a validation error")
	}
}

// TestIntegration_PushVMSnapshotTo_InvalidNameIsError confirms both
// names are validated before anything is read: the id and the snapshot
// name are interpolated into a `zfs send` argument, so a bad one must
// never reach Send. The assertion is on the fake ZFS's own record of
// what it was asked to send, which is what actually distinguishes
// "refused before reading" from "refused later, after a send
// attempt" - a pushCalls check alone cannot tell those apart.
//
// No case here is a snapshot name starting with a dash. That is legal
// and admitted on purpose: validVMSnapshotName allows '-', and no
// layer underneath ever sees such a name as the first character of an
// argument, because it is always interpolated as "<id>@<snapshot>"
// after the id itself has been validated.
func TestIntegration_PushVMSnapshotTo_InvalidNameIsError(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *rpcpb.PushVMSnapshotToRequest
	}{
		{"path traversal in the id", &rpcpb.PushVMSnapshotToRequest{Id: "../../etc/passwd", SnapshotName: "nightly", TargetNodeId: "raftd-1"}},
		{"absolute path in the id", &rpcpb.PushVMSnapshotToRequest{Id: "/etc/passwd", SnapshotName: "nightly", TargetNodeId: "raftd-1"}},
		{"shell metacharacter in the snapshot name", &rpcpb.PushVMSnapshotToRequest{Id: "vm-1", SnapshotName: "nightly; rm -rf /", TargetNodeId: "raftd-1"}},
		{"path separator in the snapshot name", &rpcpb.PushVMSnapshotToRequest{Id: "vm-1", SnapshotName: "nightly/nightly", TargetNodeId: "raftd-1"}},
		{"over-long snapshot name", &rpcpb.PushVMSnapshotToRequest{Id: "vm-1", SnapshotName: strings.Repeat("a", 65), TargetNodeId: "raftd-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raftdSocket := newRaftdUDSSocket(t)
			peers := &fakeVMSnapshotPeerForwarder{}
			zfsMgr := &fakeQuotaSetter{}
			client, _ := startManagerdWithZFS(t, raftdSocket, "manager-1", zfsMgr, peers)

			resp, err := client.PushVMSnapshotTo(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("PushVMSnapshotTo() error: %v", err)
			}
			if resp.GetError() == "" {
				t.Fatal("PushVMSnapshotTo() with an invalid name = no error, want a rejection")
			}
			if len(zfsMgr.sentSnapshots) != 0 {
				t.Errorf("zfs send called with %v, want none - the name was refused before anything was read", zfsMgr.sentSnapshots)
			}
			peers.mu.Lock()
			defer peers.mu.Unlock()
			if len(peers.pushCalls) != 0 {
				t.Errorf("PushVMSnapshot calls = %+v, want none - the name was refused before any transfer", peers.pushCalls)
			}
		})
	}
}

func TestIntegration_PushVMSnapshotTo_NoZFSConfiguredIsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClientWithZFSAndPeers(t, raftdSocket, "manager-1", nil, &fakeVMSnapshotPeerForwarder{})

	resp, err := client.PushVMSnapshotTo(context.Background(), &rpcpb.PushVMSnapshotToRequest{Id: "vm-1", SnapshotName: "nightly", TargetNodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("PushVMSnapshotTo() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("PushVMSnapshotTo() with no ZFS configured = no error, want a clear rejection")
	}
}

// TestIntegration_PushVMSnapshotTo_PushesSnapshotToResolvedTarget
// confirms a local zfs.Send is streamed to the resolved target's
// PushVMSnapshot, and that the two names travelling are the ones the
// caller asked for - the receiving end re-validates and re-publishes
// them, so a mismatch here would silently land a checkpoint under the
// wrong name.
func TestIntegration_PushVMSnapshotTo_PushesSnapshotToResolvedTarget(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	peers := &fakeVMSnapshotPeerForwarder{}
	zfsMgr := &fakeQuotaSetter{sendData: map[string]string{"vm-1@nightly": "zfs send bytes"}}
	client := newManagerdRPCClientWithZFSAndPeers(t, raftdSocket, "manager-1", zfsMgr, peers)

	resp, err := client.PushVMSnapshotTo(context.Background(), &rpcpb.PushVMSnapshotToRequest{Id: "vm-1", SnapshotName: "nightly", TargetNodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("PushVMSnapshotTo() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("PushVMSnapshotTo() error = %q, want success", resp.GetError())
	}

	peers.mu.Lock()
	defer peers.mu.Unlock()
	if len(peers.pushCalls) != 1 {
		t.Fatalf("PushVMSnapshot calls = %+v, want exactly one", peers.pushCalls)
	}
	if peers.pushCalls[0].vmID != "vm-1" || peers.pushCalls[0].snapshotName != "nightly" {
		t.Errorf("PushVMSnapshot call = %+v, want vm-1/nightly", peers.pushCalls[0])
	}
	if !strings.HasSuffix(peers.pushCalls[0].addr, ":17700") {
		t.Errorf("PushVMSnapshot addr = %q, want the raft address with the managerd port substituted", peers.pushCalls[0].addr)
	}
	if len(zfsMgr.sentSnapshots) != 1 || zfsMgr.sentSnapshots[0] != "vm-1@nightly" {
		t.Errorf("zfs send called with %v, want exactly [vm-1@nightly] read from this node's own dataset", zfsMgr.sentSnapshots)
	}
}

func TestIntegration_PushVMSnapshotTo_SendErrorIsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	peers := &fakeVMSnapshotPeerForwarder{}
	zfsMgr := &fakeQuotaSetter{sendErr: errors.New("dataset does not exist")}
	client := newManagerdRPCClientWithZFSAndPeers(t, raftdSocket, "manager-1", zfsMgr, peers)

	resp, err := client.PushVMSnapshotTo(context.Background(), &rpcpb.PushVMSnapshotToRequest{Id: "vm-1", SnapshotName: "missing", TargetNodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("PushVMSnapshotTo() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("PushVMSnapshotTo() with a Send error = no error, want a clear rejection")
	}
	peers.mu.Lock()
	defer peers.mu.Unlock()
	if len(peers.pushCalls) != 0 {
		t.Errorf("PushVMSnapshot calls = %+v, want none - Send failed before any push was attempted", peers.pushCalls)
	}
	if len(zfsMgr.sentSnapshots) != 1 || zfsMgr.sentSnapshots[0] != "vm-1@missing" {
		t.Errorf("zfs send called with %v, want exactly [vm-1@missing] - the send was attempted and is what failed", zfsMgr.sentSnapshots)
	}
}

// TestIntegration_PushVMSnapshotTo_PushErrorIsError confirms a refused
// push is surfaced rather than reported as a success with no transfer
// behind it.
func TestIntegration_PushVMSnapshotTo_PushErrorIsError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	peers := &fakeVMSnapshotPeerForwarder{pushErr: errors.New("VM vm-1 must be stopped before receiving a snapshot for it")}
	zfsMgr := &fakeQuotaSetter{sendData: map[string]string{"vm-1@nightly": "zfs send bytes"}}
	client := newManagerdRPCClientWithZFSAndPeers(t, raftdSocket, "manager-1", zfsMgr, peers)

	resp, err := client.PushVMSnapshotTo(context.Background(), &rpcpb.PushVMSnapshotToRequest{Id: "vm-1", SnapshotName: "nightly", TargetNodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("PushVMSnapshotTo() error: %v", err)
	}
	if !strings.Contains(resp.GetError(), "must be stopped") {
		t.Errorf("PushVMSnapshotTo() error = %q, want the target's refusal carried back verbatim", resp.GetError())
	}
}

// TestIntegration_PushVMSnapshot_RealClientStreamReachesReceive is the
// test that matters most on this path: it runs a real managerd as the
// target and drives it with a real *PeerReporter, so the metadata
// message, the chunking and the CloseAndRecv are the production code
// rather than a stand-in. The bytes are large enough to span several
// 256 KiB chunks, which is the thing a hand-rolled sender gets wrong.
func TestIntegration_PushVMSnapshot_RealClientStreamReachesReceive(t *testing.T) {
	payload := strings.Repeat("zfs send payload line\n", 40000)

	raftdSocket := newRaftdUDSSocket(t)
	targetZFS := &fakeQuotaSetter{snapshots: map[string]bool{}}
	// A real `zfs receive -F` publishes the snapshot as the stream
	// lands; the hook reproduces that, and it must fire after the
	// pre-receive "already exists?" check has already said no.
	targetZFS.receiveForceHook = func(destName string) { targetZFS.snapshots[destName+"@nightly"] = true }
	targetClient, targetAddr := startManagerdWithZFS(t, raftdSocket, "manager-2", targetZFS, nil)
	// The VM lives on node-b, which is not this node: the definition
	// is cluster-replicated but the dataset is not, which is exactly
	// the state a cross-Comb clone source is fetched from.
	seedVMDefinition(t, targetClient, "vm-1", "node-b")

	reporter := NewPeerReporter("", false, nil)
	if err := reporter.PushVMSnapshot(context.Background(), targetAddr, "vm-1", "nightly", strings.NewReader(payload)); err != nil {
		t.Fatalf("PushVMSnapshot() error: %v", err)
	}

	if got := targetZFS.forceReceivedInto["vm-1"]; got != payload {
		t.Errorf("bytes received into vm-1 = %d bytes, want the %d bytes that were sent", len(got), len(payload))
	}
	if !targetZFS.snapshots["vm-1@nightly"] {
		t.Error("the received snapshot was never confirmed to exist locally, so ReceiveVMSnapshot should have reported failure rather than success")
	}
}

// TestIntegration_PushVMSnapshot_RejectsMetadataIsNotChunked confirms
// the first message is always the metadata one, even for a payload
// small enough to be a single chunk: a leading chunk would fail
// ReceiveVMSnapshot's "first message must be metadata" refusal.
func TestIntegration_PushVMSnapshot_RejectsMetadataIsNotChunked(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	targetZFS := &fakeQuotaSetter{snapshots: map[string]bool{}}
	targetZFS.receiveForceHook = func(destName string) { targetZFS.snapshots[destName+"@nightly"] = true }
	targetClient, targetAddr := startManagerdWithZFS(t, raftdSocket, "manager-2", targetZFS, nil)
	seedVMDefinition(t, targetClient, "vm-1", "node-b")

	reporter := NewPeerReporter("", false, nil)
	if err := reporter.PushVMSnapshot(context.Background(), targetAddr, "vm-1", "nightly", strings.NewReader("tiny")); err != nil {
		t.Fatalf("PushVMSnapshot() error: %v", err)
	}
	if got := targetZFS.forceReceivedInto["vm-1"]; got != "tiny" {
		t.Errorf("bytes received into vm-1 = %q, want %q", got, "tiny")
	}
}

// TestIntegration_PushVMSnapshot_TargetRefusalIsAnError confirms the
// response's error field is never swallowed: a target that refuses
// (here, because it already holds that snapshot) must surface as an
// error on the pusher, not as a silent success.
func TestIntegration_PushVMSnapshot_TargetRefusalIsAnError(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	targetZFS := &fakeQuotaSetter{snapshots: map[string]bool{"vm-1@nightly": true}}
	targetClient, targetAddr := startManagerdWithZFS(t, raftdSocket, "manager-2", targetZFS, nil)
	seedVMDefinition(t, targetClient, "vm-1", "node-b")

	reporter := NewPeerReporter("", false, nil)
	err := reporter.PushVMSnapshot(context.Background(), targetAddr, "vm-1", "nightly", strings.NewReader("bytes"))
	if err == nil {
		t.Fatal("PushVMSnapshot() = nil error, want the target's refusal surfaced")
	}
	if !strings.Contains(err.Error(), "already has a snapshot") {
		t.Errorf("PushVMSnapshot() error = %q, want the target's reason carried back", err)
	}
}

// TestIntegration_RequestVMSnapshotPush_CallsTheTargetsOwnRPC drives the
// other half of the pair: the reconciler-facing call, which asks a
// specific peer to push to a specific node rather than streaming
// itself.
func TestIntegration_RequestVMSnapshotPush_CallsTheTargetsOwnRPC(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	peers := &fakeVMSnapshotPeerForwarder{}
	zfsMgr := &fakeQuotaSetter{sendData: map[string]string{"vm-1@nightly": "zfs send bytes"}}
	_, addr := startManagerdWithZFS(t, raftdSocket, "manager-1", zfsMgr, peers)

	reporter := NewPeerReporter("", false, nil)
	if err := reporter.RequestVMSnapshotPush(context.Background(), addr, "vm-1", "nightly", "raftd-1"); err != nil {
		t.Fatalf("RequestVMSnapshotPush() error: %v", err)
	}

	peers.mu.Lock()
	gotPushes := append([]struct{ addr, vmID, snapshotName string }{}, peers.pushCalls...)
	peers.mu.Unlock()
	if len(gotPushes) != 1 || gotPushes[0].vmID != "vm-1" || gotPushes[0].snapshotName != "nightly" {
		t.Errorf("PushVMSnapshot calls = %+v, want exactly one for vm-1/nightly", gotPushes)
	}

	// And the same reporter surfaces the target's refusal rather than
	// reporting a request that did nothing. The fake's own lock is not
	// held across this call: the refusal it records is recorded on the
	// managerd's goroutine, inside the RPC that is still running here.
	peers.mu.Lock()
	peers.pushErr = errors.New("peer forwarding is not configured on this node")
	peers.mu.Unlock()
	if err := reporter.RequestVMSnapshotPush(context.Background(), addr, "vm-1", "nightly", "raftd-1"); err == nil {
		t.Fatal("RequestVMSnapshotPush() = nil error, want the target's refusal surfaced")
	}
}

// TestIntegration_ListVMSnapshotNames_ReportsTheTargetsCheckpoints
// covers the discovery half the reconciler uses to decide which peer to
// ask, including the per-VM scoping that the ISO and base-template
// equivalents do not have.
func TestIntegration_ListVMSnapshotNames_ReportsTheTargetsCheckpoints(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	zfsMgr := &fakeQuotaSetter{snapshots: map[string]bool{"vm-1@nightly": true, "vm-1@weekly": true, "vm-2@nightly": true}}
	_, addr := startManagerdWithZFS(t, raftdSocket, "manager-2", zfsMgr, nil)

	reporter := NewPeerReporter("", false, nil)
	names, err := reporter.ListVMSnapshotNames(context.Background(), addr, "vm-1")
	if err != nil {
		t.Fatalf("ListVMSnapshotNames() error: %v", err)
	}
	if len(names) != 2 || !containsString(names, "nightly") || !containsString(names, "weekly") {
		t.Errorf("ListVMSnapshotNames(vm-1) = %v, want nightly and weekly", names)
	}
	names, err = reporter.ListVMSnapshotNames(context.Background(), addr, "vm-9")
	if err != nil {
		t.Fatalf("ListVMSnapshotNames() error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("ListVMSnapshotNames(vm-9) = %v, want none - a peer does not answer for a VM it has never heard of", names)
	}
}

// TestIntegration_PushVMSnapshot_UnknownVMIsRefused covers the
// raft-backed precondition a plain unit test with no raft client
// cannot reach: a snapshot for a VM this node has no replicated
// definition of would create a dataset nothing could ever attach to,
// so it is refused before a byte is read.
func TestIntegration_PushVMSnapshot_UnknownVMIsRefused(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	targetZFS := &fakeQuotaSetter{snapshots: map[string]bool{}}
	_, targetAddr := startManagerdWithZFS(t, raftdSocket, "manager-2", targetZFS, nil)
	// Deliberately no CreateVM: vm-1 is defined nowhere in this raft.

	reporter := NewPeerReporter("", false, nil)
	err := reporter.PushVMSnapshot(context.Background(), targetAddr, "vm-1", "nightly", strings.NewReader("bytes"))
	if err == nil {
		t.Fatal("PushVMSnapshot() = nil error, want a refusal for a VM this node has no definition of")
	}
	if !strings.Contains(err.Error(), "no replicated definition") {
		t.Errorf("PushVMSnapshot() error = %q, want the unknown-VM reason carried back", err)
	}
	if targetZFS.forceReceivedInto != nil {
		t.Errorf("ReceiveForce called with %v, want none - refused before any transfer", targetZFS.forceReceivedInto)
	}
}

// TestIntegration_PushVMSnapshot_RunningVMWithALocalDatasetIsRefused
// pins the running-VM half of the receive preconditions against a
// real raft, where the VM is genuinely desired-RUNNING. The refusal
// is driven by the TARGET still holding the dataset, not by the
// running VM: this is a -F receive, which rewrites a dataset a bhyve
// process on this node has open, and that is the only thing it
// damages.
func TestIntegration_PushVMSnapshot_RunningVMWithALocalDatasetIsRefused(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	targetZFS := &fakeQuotaSetter{
		snapshots: map[string]bool{},
		datasets:  []string{"vm-1"},
	}
	targetZFS.receiveForceHook = func(destName string) { targetZFS.snapshots[destName+"@nightly"] = true }
	targetClient, targetAddr := startManagerdWithZFS(t, raftdSocket, "manager-2", targetZFS, nil)
	seedVMWithState(t, targetClient, "vm-1", "node-b", rpcpb.VMState_VM_STATE_RUNNING)

	reporter := NewPeerReporter("", false, nil)
	err := reporter.PushVMSnapshot(context.Background(), targetAddr, "vm-1", "nightly", strings.NewReader("bytes"))
	if err == nil {
		t.Fatal("PushVMSnapshot() = nil error, want a refusal while this node still holds the running VM's dataset")
	}
	if !strings.Contains(err.Error(), "must be stopped") {
		t.Errorf("PushVMSnapshot() error = %q, want the running-VM reason carried back", err)
	}
	if targetZFS.forceReceivedInto != nil {
		t.Errorf("ReceiveForce called with %v, want none - a -F receive would have rewritten a live disk", targetZFS.forceReceivedInto)
	}
}

// TestIntegration_PushVMSnapshot_RunningVMOnAnotherCombIsAccepted is
// the counterpart, and it is the case this whole pair exists for. A
// VM desired-RUNNING on node-b, whose dataset this node does not
// hold, has no bhyve process here and no local disk for `zfs
// receive -F` to rewrite - it would create the dataset, not roll one
// back. Refusing on the replicated desired state alone would make the
// cross-Comb clone-source fetch impossible for every source VM that
// is running on its own Comb, which is the ordinary case, while
// protecting nothing.
func TestIntegration_PushVMSnapshot_RunningVMOnAnotherCombIsAccepted(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	// No `datasets` entry and no existsOverride: this node holds no
	// disk for vm-1 at all.
	targetZFS := &fakeQuotaSetter{snapshots: map[string]bool{}}
	targetZFS.receiveForceHook = func(destName string) { targetZFS.snapshots[destName+"@nightly"] = true }
	targetClient, targetAddr := startManagerdWithZFS(t, raftdSocket, "manager-2", targetZFS, nil)
	seedVMWithState(t, targetClient, "vm-1", "node-b", rpcpb.VMState_VM_STATE_RUNNING)

	reporter := NewPeerReporter("", false, nil)
	if err := reporter.PushVMSnapshot(context.Background(), targetAddr, "vm-1", "nightly", strings.NewReader("bytes")); err != nil {
		t.Fatalf("PushVMSnapshot() error: %v, want the transfer to go through for a VM this node holds no dataset for", err)
	}
	if got := targetZFS.forceReceivedInto["vm-1"]; got != "bytes" {
		t.Errorf("bytes received into vm-1 = %q, want %q", got, "bytes")
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
