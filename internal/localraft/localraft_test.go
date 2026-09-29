package localraft_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/localraft"
	"github.com/glenjbarber/apiary/internal/raft"
)

// This file is about the one property that matters to the two commands
// that use this package, and it is a property about FAILURE.
//
// `apiaryctl force-restart` refuses on "this Comb is the leader" and
// also refuses on "I could not find out whether this Comb is the
// leader". If a dial against a dead raftd, a socket path naming some
// other Comb, or an internal token that does not match returned a
// zero-value Status and a nil error, the second refusal would be
// unreachable, the first would be the only one, and the check would be
// decorative - in exactly the state an incident produces. So these
// cases stand up a real gRPC server on a real unix socket and assert
// what comes back both when it answers and when it cannot.
//
// The server is a stub, not a raft node. What is under test is the
// dial: the config resolution, the token attachment, the field mapping,
// and above all the difference between an error and a false.

const testToken = "s3cr3t-internal-token"

// stubRaftd serves exactly the one RPC this package calls, over a real
// listener on a real socket, with a real token interceptor.
type stubRaftd struct {
	internalpb.UnimplementedRaftInternalServer

	// isLeader, nodeID, raftState and leaderID are what Status answers
	// with. They are held as plain fields rather than as a
	// StatusResponse because a protobuf message carries a mutex, and
	// copying one out of a struct is a vet error and a real one: the
	// copy shares state with the original.
	isLeader  bool
	nodeID    string
	raftState string
	leaderID  string

	// wantToken is the token the server requires. An empty string
	// disables the check, which is the real raftd's behaviour when
	// -internal-token is not used.
	wantToken string

	mu       sync.Mutex
	sawAuth  []string
	callsFor int
}

func (s *stubRaftd) Status(ctx context.Context, _ *internalpb.StatusRequest) (*internalpb.StatusResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	s.sawAuth = append(s.sawAuth, strings.Join(md.Get("authorization"), ","))
	s.callsFor++
	s.mu.Unlock()
	return &internalpb.StatusResponse{
		IsLeader:  s.isLeader,
		NodeId:    s.nodeID,
		RaftState: s.raftState,
		LeaderId:  s.leaderID,
	}, nil
}

// serve starts a stubRaftd on a unix socket inside a temp directory and
// returns its path plus a raftd.json naming it. It returns a stop
// function that must be called.
func serve(t *testing.T, s *stubRaftd) (raftdJSON string, stop func()) {
	t.Helper()
	dir := t.TempDir()
	// os.MkdirTemp with a two-character pattern, not t.TempDir: the
	// latter embeds the test name and on macOS runs past the 104-byte
	// sun_path limit well before the name does. internal/raft's own
	// integration test solves this the same way.
	short, err := os.MkdirTemp("", "lr")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	sock := filepath.Join(short, "raftd.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listening on %s: %v", sock, err)
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(raft.TokenUnaryInterceptor(s.wantToken)))
	internalpb.RegisterRaftInternalServer(srv, s)
	go func() { _ = srv.Serve(lis) }()

	cfg := `{"socket":` + quote(sock)
	if s.wantToken != "" {
		cfg += `,"internal_token":` + quote(s.wantToken)
	}
	cfg += "}\n"
	raftdJSON = filepath.Join(dir, "raftd.json")
	if err := os.WriteFile(raftdJSON, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing fixture raftd.json: %v", err)
	}
	return raftdJSON, func() {
		srv.Stop()
		_ = os.Remove(sock)
	}
}

func quote(s string) string { return `"` + s + `"` }

func ctxWithTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A live raftd that says it is the leader must be reported as exactly
// that, and the refusal that depends on it is only as good as this
// mapping.
func TestQuery_ReportsARealAnswer(t *testing.T) {
	stub := &stubRaftd{isLeader: true, nodeID: "brood", raftState: "Leader", leaderID: "brood"}
	raftdJSON, stop := serve(t, stub)
	defer stop()

	got, err := localraft.Query(ctxWithTimeout(t), raftdJSON)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !got.IsLeader {
		t.Error("IsLeader = false, want true: this is the answer force-restart refuses on, and it is the one that must never be lost in a mapping")
	}
	if got.NodeID != "brood" || got.RaftState != "Leader" || got.LeaderID != "brood" {
		t.Errorf("Query = %+v, want node brood, state Leader, leader brood", got)
	}
}

// A follower is a follower. The check has to be able to say no, or
// force-restart refuses everywhere and the guard is a denial of
// service rather than a guard.
func TestQuery_ReportsAFollower(t *testing.T) {
	stub := &stubRaftd{isLeader: false, nodeID: "drone", raftState: "Follower", leaderID: "brood"}
	raftdJSON, stop := serve(t, stub)
	defer stop()

	got, err := localraft.Query(ctxWithTimeout(t), raftdJSON)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.IsLeader {
		t.Error("IsLeader = true, want false")
	}
	// LeaderID is the field that proves this is a real raft answer and
	// not a hardcoded false: a follower knows who the leader is, and a
	// check that could not read that would be reading nothing.
	if got.LeaderID != "brood" {
		t.Errorf("LeaderID = %q, want %q: a follower's knowledge of the leader is carried, not dropped", got.LeaderID, "brood")
	}
}

// The token has to be presented, from the config that also named the
// socket. These were two independent reads in apiaryctl join-authorize
// before this package existed, and a Comb configured with
// -internal-token on a non-default path was one where the two could
// disagree.
func TestQuery_PresentsTheInternalTokenFromTheSameConfig(t *testing.T) {
	stub := &stubRaftd{wantToken: testToken}
	raftdJSON, stop := serve(t, stub)
	defer stop()

	if _, err := localraft.Query(ctxWithTimeout(t), raftdJSON); err != nil {
		t.Fatalf("Query: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.sawAuth) != 1 || stub.sawAuth[0] != "Bearer "+testToken {
		t.Errorf("authorization metadata seen = %q, want exactly one call carrying %q", stub.sawAuth, "Bearer "+testToken)
	}
}

// THE CASE THE WHOLE REFUSAL RESTS ON.
//
// A Comb whose raftd cannot answer is a Comb whose leadership is not
// established, and force-restart restarts raftd. So this must be an
// error, not a Status with IsLeader false. A zero Status and a nil
// error here would make the unknown-leadership refusal unreachable code
// and turn the leader check into a check that passes precisely when it
// could not be performed.
func TestQuery_AnUnreachableRaftdIsAnErrorNotAFalse(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "raftd.sock")
	// The socket is never created. A missing socket is what a Comb
	// whose raftd was never started looks like from here, and it is not
	// a rarer case than a wrong one.
	raftdJSON := filepath.Join(dir, "raftd.json")
	if err := os.WriteFile(raftdJSON, []byte(`{"socket":`+quote(sock)+`}`+"\n"), 0o600); err != nil {
		t.Fatalf("writing fixture raftd.json: %v", err)
	}

	got, err := localraft.Query(ctxWithTimeout(t), raftdJSON)
	if err == nil {
		t.Fatalf("Query on a socket with nothing behind it returned %+v and no error; a caller that treats this as 'not the leader' has no guard at all", got)
	}
	if got != (localraft.Status{}) {
		t.Errorf("Query returned %+v alongside its error; the fields must be zero so a caller that ignores the error cannot act on them", got)
	}
	// The message has to name the thing that is wrong, because the
	// operator's next action is to go and look at that thing.
	if !strings.Contains(err.Error(), sock) {
		t.Errorf("error %q does not name the socket it could not reach (%q)", err, sock)
	}
}

// A token raftd rejects is the same class of problem as a dead raftd,
// and it is worth a case of its own because the failure looks different
// on the wire: Unauthenticated, not Unavailable.
func TestQuery_ARejectedTokenIsAnErrorNotAFalse(t *testing.T) {
	stub := &stubRaftd{isLeader: true, wantToken: testToken}
	raftdJSON, stop := serve(t, stub)
	defer stop()

	// Same socket, a config whose token is wrong.
	wrong := filepath.Join(filepath.Dir(raftdJSON), "wrong-token.json")
	body, err := os.ReadFile(raftdJSON)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if err := os.WriteFile(wrong, []byte(strings.Replace(string(body), testToken, "not-the-token", 1)), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	got, err := localraft.Query(ctxWithTimeout(t), wrong)
	if err == nil {
		t.Fatalf("Query against a raftd that rejects the token returned %+v and no error", got)
	}
	if !strings.Contains(err.Error(), "Unauthenticated") {
		t.Errorf("error %q does not carry the Unauthenticated code; an operator reading it should be able to tell a wrong token from a dead daemon", err)
	}
}

// A config that names no socket resolves to raftd's OWN default,
// through raftd's own loader. That is the right answer and it is worth
// pinning: a Comb whose raftd.json carries only a node_id is read at
// /var/run/apiary/raftd.sock, the same path raftd itself opens, and a
// resolution that invented a different one would have the leader check
// asking some other Comb's raft whether this Comb is the leader.
//
// The default is unreachable, so the query fails - and it must fail as
// an error, which is the same property as the missing-socket case above
// and the reason it is worth a case of its own.
func TestQuery_ConfigWithNoSocketResolvesToRaftdOwnDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raftd.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	// No server is listening at raftd's default socket on a developer
	// machine, and the assertion is on the error rather than on the
	// path: what must not happen is a silent success or, worse, a
	// Status with IsLeader false.
	got, err := localraft.Query(ctxWithTimeout(t), path)
	if err == nil {
		t.Fatalf("Query resolved to something and answered %+v; nothing is listening at raftd's default socket here", got)
	}
	if got != (localraft.Status{}) {
		t.Errorf("Query returned %+v alongside its error; the fields must be zero", got)
	}
}

// An unreadable config is a refusal naming the path. The same applies
// to a config that exists but is not JSON, which is what a hand-edited
// raftd.json on a Comb with no checkout looks like.
func TestDial_UnreadableConfigIsRefusedAndNamesThePath(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json at all", "this is not json\n"},
		{"empty file", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "raftd.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("writing fixture: %v", err)
			}
			_, err := localraft.Dial(path)
			if err == nil {
				t.Fatal("Dial succeeded against an unreadable config")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name the config path %q", err, path)
			}
		})
	}
}
