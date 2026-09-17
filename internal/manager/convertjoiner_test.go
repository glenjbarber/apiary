package manager

import (
	"context"
	"errors"
	"fmt"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// newExistingRaftStateDir creates genuine on-disk raft state (via the
// real internal/raft.New/Bootstrap, the same primitives cmd/raftd itself
// uses) and returns its data directory - the only reliable way to get
// raftnode.HasExistingState to report true, since it inspects the actual
// BoltDB log/stable store contents, not just file presence.
func newExistingRaftStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	node, err := raftnode.New(raftnode.Config{NodeID: "node-1", DataDir: dir, BindAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}
	if err := node.Bootstrap(); err != nil {
		node.Shutdown()
		t.Fatalf("Bootstrap() error: %v", err)
	}
	node.Shutdown()
	return dir
}

type fakeRaftdConversionConfigStore struct {
	cfg      raftdconfig.Config
	loadErr  error
	saveErr  error
	saved    *raftdconfig.Config
	loadCall int
}

func (f *fakeRaftdConversionConfigStore) Load() (raftdconfig.Config, error) {
	f.loadCall++
	if f.loadErr != nil {
		return raftdconfig.Config{}, f.loadErr
	}
	return f.cfg, nil
}

func (f *fakeRaftdConversionConfigStore) Save(cfg raftdconfig.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	c := cfg
	f.saved = &c
	return nil
}

type fakeRaftdConversionController struct {
	stopErr, startErr, listenErr error
	resetBackup                  string
	resetErr                     error
	listening                    bool

	stopCalled, startCalled, resetCalled bool
	listenAddr                           string
}

func (f *fakeRaftdConversionController) Stop(context.Context) error {
	f.stopCalled = true
	return f.stopErr
}

func (f *fakeRaftdConversionController) Reset(context.Context) (string, error) {
	f.resetCalled = true
	if f.resetErr != nil {
		return f.resetBackup, f.resetErr
	}
	return f.resetBackup, nil
}

func (f *fakeRaftdConversionController) Start(context.Context) error {
	f.startCalled = true
	return f.startErr
}

func (f *fakeRaftdConversionController) IsListening(_ context.Context, addr string) (bool, error) {
	f.listenAddr = addr
	if f.listenErr != nil {
		return false, f.listenErr
	}
	return f.listening, nil
}

// newConvertJoinerTestServer wires a Server with every ConvertStandaloneToJoiner
// dependency defaulted to a working, permissive fake - individual tests
// override only the field(s) they care about.
func newConvertJoinerTestServer(t *testing.T) (*Server, *fakeRaftdConversionConfigStore, *fakeRaftdConversionController, *fakeJoinColonyPeerForwarder) {
	t.Helper()
	dir := newExistingRaftStateDir(t)
	cfgStore := &fakeRaftdConversionConfigStore{cfg: raftdconfig.Config{
		DataDir: dir, Socket: "/var/run/apiary/raftd.sock", NodeID: "node-1",
		RaftBind: "10.90.0.10:17600", InternalToken: "secret-token",
	}}
	ctrl := &fakeRaftdConversionController{listening: true}
	peers := &fakeJoinColonyPeerForwarder{}

	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	s.raftdConversionConfig = cfgStore
	s.raftdConversion = ctrl
	s.reachabilityCheck = func(context.Context, string) error { return nil }
	return s, cfgStore, ctrl, peers
}

func validConvertReq() *rpcpb.ConvertStandaloneToJoinerRequest {
	return &rpcpb.ConvertStandaloneToJoinerRequest{
		TargetManagerdAddress: "10.90.0.1:17700",
		RaftBind:              "10.90.0.20:17600",
		ConfirmPhrase:         convertToJoinerConfirmPhrase,
	}
}

func TestConvertStandaloneToJoiner_WrongPhraseRejected(t *testing.T) {
	s, _, ctrl, _ := newConvertJoinerTestServer(t)
	req := validConvertReq()
	req.ConfirmPhrase = "not-the-phrase"

	resp, err := s.ConvertStandaloneToJoiner(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("response = %+v, want an error for a wrong confirm_phrase", resp)
	}
	if ctrl.stopCalled || ctrl.resetCalled || ctrl.startCalled {
		t.Errorf("adapter methods were called for a rejected confirm_phrase: %+v", ctrl)
	}
}

func TestConvertStandaloneToJoiner_UnreachableTargetRejectedBeforeAnyAction(t *testing.T) {
	s, _, ctrl, _ := newConvertJoinerTestServer(t)
	s.reachabilityCheck = func(context.Context, string) error { return errors.New("connection refused") }

	resp, err := s.ConvertStandaloneToJoiner(context.Background(), validConvertReq())
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("response = %+v, want an error for an unreachable target", resp)
	}
	if ctrl.stopCalled || ctrl.resetCalled || ctrl.startCalled {
		t.Errorf("adapter methods were called before the reachability check passed: %+v", ctrl)
	}
}

func TestConvertStandaloneToJoiner_InvalidRaftBindRejected(t *testing.T) {
	s, _, ctrl, _ := newConvertJoinerTestServer(t)
	for _, bad := range []string{"", "0.0.0.0:17600", "127.0.0.1:17600", "localhost:17600", "not-a-host-port"} {
		req := validConvertReq()
		req.RaftBind = bad
		resp, err := s.ConvertStandaloneToJoiner(context.Background(), req)
		if err != nil {
			t.Fatalf("raft_bind=%q: unexpected transport error: %v", bad, err)
		}
		if resp.GetError() == "" {
			t.Errorf("raft_bind=%q: response = %+v, want an error", bad, resp)
		}
	}
	if ctrl.stopCalled {
		t.Errorf("adapter was called for an invalid raft_bind")
	}
}

func TestConvertStandaloneToJoiner_NoExistingStateRejectedAndRestarted(t *testing.T) {
	s, cfgStore, ctrl, _ := newConvertJoinerTestServer(t)
	cfgStore.cfg.DataDir = t.TempDir() // freshly empty, never bootstrapped

	resp, err := s.ConvertStandaloneToJoiner(context.Background(), validConvertReq())
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("response = %+v, want an error for an empty data directory", resp)
	}
	if !ctrl.stopCalled {
		t.Errorf("Stop was never called even though a stop-then-check sequence is required to inspect state safely")
	}
	if !ctrl.startCalled {
		t.Errorf("raftd was not restarted after refusing a no-state conversion - Comb was left down")
	}
	if ctrl.resetCalled {
		t.Errorf("Reset was called despite no existing state to convert")
	}
}

func TestConvertStandaloneToJoiner_FailedStopLeavesNoReadyResult(t *testing.T) {
	s, _, ctrl, _ := newConvertJoinerTestServer(t)
	ctrl.stopErr = errors.New("service apiary_raftd stop: exit status 1")

	resp, err := s.ConvertStandaloneToJoiner(context.Background(), validConvertReq())
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" || resp.GetJoinRequestId() != "" || resp.GetJoinRequestCode() != "" {
		t.Fatalf("response = %+v, want only an error and no join-request fields set", resp)
	}
	if ctrl.resetCalled || ctrl.startCalled {
		t.Errorf("later stages ran despite Stop failing: %+v", ctrl)
	}
}

func TestConvertStandaloneToJoiner_FailedResetReportsNoBackupAndLeavesRaftdStopped(t *testing.T) {
	s, _, ctrl, _ := newConvertJoinerTestServer(t)
	ctrl.resetErr = errors.New("raftd -reset: exit status 1")

	resp, err := s.ConvertStandaloneToJoiner(context.Background(), validConvertReq())
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("response = %+v, want an error", resp)
	}
	if resp.GetJoinRequestId() != "" {
		t.Errorf("response = %+v, want no join-request id on a failed reset", resp)
	}
	if ctrl.startCalled {
		t.Errorf("Start was called after a failed Reset - raftd should be left stopped, not silently restarted with unknown state")
	}
}

func TestConvertStandaloneToJoiner_FailedSaveReportsBackupPath(t *testing.T) {
	s, cfgStore, ctrl, _ := newConvertJoinerTestServer(t)
	ctrl.resetBackup = "/var/db/apiary/raftd.reset-backup-1234567890"
	cfgStore.saveErr = errors.New("disk full")

	resp, err := s.ConvertStandaloneToJoiner(context.Background(), validConvertReq())
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("response = %+v, want an error", resp)
	}
	if resp.GetBackupDataDir() != ctrl.resetBackup {
		t.Errorf("BackupDataDir = %q, want %q (the reset already happened before Save failed)", resp.GetBackupDataDir(), ctrl.resetBackup)
	}
	if ctrl.startCalled {
		t.Errorf("Start was called after a failed config Save")
	}
}

func TestConvertStandaloneToJoiner_FailedStartReportsBackupPath(t *testing.T) {
	s, _, ctrl, _ := newConvertJoinerTestServer(t)
	ctrl.resetBackup = "/var/db/apiary/raftd.reset-backup-1234567890"
	ctrl.startErr = errors.New("service apiary_raftd start: exit status 1")

	resp, err := s.ConvertStandaloneToJoiner(context.Background(), validConvertReq())
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" || resp.GetBackupDataDir() != ctrl.resetBackup {
		t.Fatalf("response = %+v, want an error and the backup path reported", resp)
	}
	if resp.GetJoinRequestId() != "" {
		t.Errorf("response = %+v, want no join request submitted after a failed Start", resp)
	}
}

func TestConvertStandaloneToJoiner_ListenTimeoutFailsClosed(t *testing.T) {
	s, _, ctrl, peers := newConvertJoinerTestServer(t)
	ctrl.listening = false

	resp, err := s.ConvertStandaloneToJoiner(context.Background(), validConvertReq())
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("response = %+v, want an error when raftd never comes up listening", resp)
	}
	if peers.lastReq != nil {
		t.Errorf("a join request was submitted despite raftd never being confirmed listening: %+v", peers.lastReq)
	}
}

func TestConvertStandaloneToJoiner_SuccessSubmitsCurrentIdentityAndReportsCode(t *testing.T) {
	s, cfgStore, ctrl, peers := newConvertJoinerTestServer(t)
	ctrl.resetBackup = "/var/db/apiary/raftd.reset-backup-1234567890"
	peers.requestResp = &rpcpb.RequestJoinColonyResponse{RequestId: "jreq-abc", Code: "654321"}

	req := validConvertReq()
	resp, err := s.ConvertStandaloneToJoiner(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("response = %+v, want no error on success", resp)
	}
	if resp.GetBackupDataDir() != ctrl.resetBackup {
		t.Errorf("BackupDataDir = %q, want %q", resp.GetBackupDataDir(), ctrl.resetBackup)
	}
	if resp.GetJoinRequestId() != "jreq-abc" || resp.GetJoinRequestCode() != "654321" {
		t.Errorf("response = %+v, want the join request id/code relayed verbatim", resp)
	}

	// Regression: the identity submitted must be this call's own current
	// node_id/raft_bind, never a cached or compiled-in value.
	if peers.lastReq.GetNodeId() != cfgStore.cfg.NodeID {
		t.Errorf("submitted node_id = %q, want the current config's %q", peers.lastReq.GetNodeId(), cfgStore.cfg.NodeID)
	}
	if peers.lastReq.GetRaftBindAddress() != req.GetRaftBind() {
		t.Errorf("submitted raft_bind_address = %q, want the request's own new value %q", peers.lastReq.GetRaftBindAddress(), req.GetRaftBind())
	}
	if peers.lastAddr != req.GetTargetManagerdAddress() {
		t.Errorf("forwarded to %q, want the request's own target %q", peers.lastAddr, req.GetTargetManagerdAddress())
	}

	// The saved config must preserve every untouched field exactly and
	// only change raft_bind/await_join/join.
	if cfgStore.saved == nil {
		t.Fatalf("Save was never called")
	}
	if cfgStore.saved.Socket != cfgStore.cfg.Socket || cfgStore.saved.NodeID != cfgStore.cfg.NodeID ||
		cfgStore.saved.InternalToken != cfgStore.cfg.InternalToken || cfgStore.saved.DataDir != cfgStore.cfg.DataDir {
		t.Errorf("saved config = %+v, want every untouched field preserved from %+v", cfgStore.saved, cfgStore.cfg)
	}
	if cfgStore.saved.RaftBind != req.GetRaftBind() {
		t.Errorf("saved raft_bind = %q, want %q", cfgStore.saved.RaftBind, req.GetRaftBind())
	}
	if !cfgStore.saved.AwaitJoin {
		t.Errorf("saved await_join = false, want true")
	}
	if cfgStore.saved.Join != "" {
		t.Errorf("saved join = %q, want empty (mutually exclusive with await_join)", cfgStore.saved.Join)
	}
	if ctrl.listenAddr != req.GetRaftBind() {
		t.Errorf("IsListening was checked against %q, want the new raft_bind %q", ctrl.listenAddr, req.GetRaftBind())
	}
}

func TestConvertStandaloneToJoiner_UnreadableLocalConfigFailsClosed(t *testing.T) {
	s, cfgStore, ctrl, _ := newConvertJoinerTestServer(t)
	cfgStore.loadErr = fmt.Errorf("raftdconfig: parsing /usr/local/etc/apiary/raftd.json: unexpected end of JSON input")

	resp, err := s.ConvertStandaloneToJoiner(context.Background(), validConvertReq())
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("response = %+v, want an error for an unreadable config", resp)
	}
	if ctrl.stopCalled {
		t.Errorf("raftd was stopped despite an unreadable local config - nothing should have been touched")
	}
}

func TestConvertStandaloneToJoiner_NoAdapterConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := s.ConvertStandaloneToJoiner(context.Background(), validConvertReq())
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("response = %+v, want an error when no conversion support is wired", resp)
	}
}
