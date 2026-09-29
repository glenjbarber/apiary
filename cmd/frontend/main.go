// Command frontend serves Apiary's HTMX web UI. It is a client of
// managerd's external gRPC API (api/rpc.ManagerService), the same way
// internal/restshim is - it never talks to raftd directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/glenjbarber/apiary/internal/buildinfo"
	"log"
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/frontend"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/httpserver"
	"github.com/glenjbarber/apiary/internal/loginconfig"
	"github.com/glenjbarber/apiary/internal/manager"
	"github.com/glenjbarber/apiary/internal/managerlink"
	"github.com/glenjbarber/apiary/internal/tlsdial"
)

// statusRetryAttempts/statusRetryDelay bound how long checkPAMConfigured
// waits for managerd to become reachable at frontend's own startup - see
// that function's own doc comment for why this exists at all.
const (
	statusRetryAttempts = 5
	statusRetryDelay    = time.Second

	// managerCheckGrace bounds how long startup waits for managerd to
	// answer the transport-scheme check before serving anyway. Same
	// value, and the same reasoning, as cmd/restshimd: long enough to
	// cover managerd starting a moment later (the two are co-located
	// and either can win the race), short enough that a frontend
	// restart never hangs on a managerd that is not coming back. A
	// managerd that does answer - the normal case - is one loopback
	// connect and is done in well under a millisecond.
	managerCheckGrace = 5 * time.Second
)

// verifier is the startup check cmd/frontend depends on, as an interface
// so the decision it makes - refuse to start, or start degraded and say
// so - can be tested without a live managerd and without a real socket.
type verifier interface {
	Verify(ctx context.Context, grace time.Duration) error
}

// apiKeyCredentials attaches an API key to every outgoing managerd call
// as gRPC metadata, matching the "authorization: Bearer <key>"
// convention internal/manager's auth interceptor expects (ADR-0023).
// RequireTransportSecurity is false to match this project's existing
// insecure local-network transport (see grpc.WithTransportCredentials
// below) - the key travels in plaintext the same way everything else
// on this connection already does.
type apiKeyCredentials string

func (k apiKeyCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(k)}, nil
}

func (apiKeyCredentials) RequireTransportSecurity() bool { return false }

// remoteAuthenticator satisfies frontend.Authenticator by asking
// managerd's own AuthenticatePassword RPC (ADR-0087) - the raw PAM
// check now happens there, not in this process, so cmd/frontend
// itself needs no cgo/native-FreeBSD build anymore.
type remoteAuthenticator struct {
	client rpcpb.ManagerServiceClient
}

func (a remoteAuthenticator) Authenticate(username, password string) (bool, error) {
	resp, err := a.client.AuthenticatePassword(context.Background(), &rpcpb.AuthenticatePasswordRequest{Username: username, Password: password})
	if err != nil {
		return false, err
	}
	if resp.GetError() != "" {
		return false, fmt.Errorf("%s", resp.GetError())
	}
	return resp.GetOk(), nil
}

// checkPAMConfigured calls managerd's Status RPC to learn whether PAM
// login is configured, retrying up to attempts times (sleeping delay
// between each) rather than giving up after the first failure - a real
// race found live: managerd itself can take a few seconds to finish
// connecting to raftd's own internal socket before it starts listening
// on its external API at all, so a single attempt made right at
// frontend's own startup can lose that race purely on timing, even
// though managerd comes up fine moments later. Whatever this returns
// is cached by the caller for frontend's entire runtime (see run()'s
// own comment on why) - there is no later recheck, so getting this
// right at startup matters more than it would if it were just retried
// per-request. An exhausted retry budget is an error, not evidence that
// PAM is disabled: callers must fail closed rather than start an
// unauthenticated frontend when managerd's authentication state is unknown.
func checkPAMConfigured(client rpcpb.ManagerServiceClient, attempts int, delay time.Duration) (configured bool, err error) {
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(delay)
		}
		resp, statusErr := client.Status(context.Background(), &rpcpb.StatusRequest{})
		if statusErr == nil {
			return resp.GetPamConfigured(), nil
		}
		err = statusErr
	}
	return false, err
}

// managerAuthenticator turns managerd's authoritative PAM status into the
// frontend authentication mode. An explicit "not configured" response keeps
// the supported no-login mode; an unavailable or indeterminate response is
// not interchangeable with that deliberate choice, so startup must fail.
func managerAuthenticator(client rpcpb.ManagerServiceClient, attempts int, delay time.Duration) (frontend.Authenticator, error) {
	configured, err := checkPAMConfigured(client, attempts, delay)
	if err != nil {
		return nil, fmt.Errorf("checking managerd login configuration after %d attempts: %w", attempts, err)
	}
	if !configured {
		return nil, nil
	}
	return remoteAuthenticator{client: client}, nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("frontend: %v", err)
	}
}

func run() error {
	// These two never had flags of their own, so they never called
	// flag.Parse. Registering -version is what introduces the first
	// one; the parse has to happen for it to take effect at all.
	buildinfo.RegisterVersionFlag(flag.CommandLine)
	flag.Parse()

	if buildinfo.VersionRequested() {
		fmt.Print(buildinfo.Report("frontend"))
		return nil
	}

	cfg, err := (&frontendconfig.Manager{}).Load()
	if err != nil {
		return fmt.Errorf("reading %s: %w", frontendconfig.DefaultPath, err)
	}

	managerCreds, err := tlsdial.ManagerDialOption(cfg.ManagerTLS, cfg.ManagerTLSCA, cfg.ManagerTLSServerName)
	if err != nil {
		return err
	}
	dialOpts := []grpc.DialOption{managerCreds}
	// ManagerAPIKey authenticates every call to managerd once it has
	// API-key auth enabled (ADR-0023) - unset by default, since auth
	// is opt-in and off until the first key is ever created via the
	// /apikeys page. Once that happens, this must be set (and frontend
	// restarted) or every call starts failing Unauthenticated. The
	// key's own role (ADR-0030) must be at least Operator, since
	// frontend forwards whatever the logged-in user's role permits -
	// a frontend whose own key is only Viewer would reject every
	// Operator/Admin action downstream regardless of the UI session.
	if cfg.ManagerAPIKey != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(apiKeyCredentials(cfg.ManagerAPIKey)))
	}
	conn, err := grpc.NewClient(cfg.ManagerAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("dialing managerd at %s: %w", cfg.ManagerAddr, err)
	}
	defer conn.Close()

	// grpc.NewClient is lazy - it resolves the target and returns a
	// ClientConn without ever opening a socket - so a disagreement
	// between this file's manager_tls setting and what managerd actually
	// speaks cannot possibly show up at the construction above. Left
	// unchecked it surfaces on the very first RPC below, as "error
	// reading server preface: EOF", with nothing in either daemon's log
	// saying the two config files disagreed. Ask managerd what it speaks
	// before authenticating against it, and long before serving anything.
	link := managerlink.New(managerlink.Config{
		Addr:        cfg.ManagerAddr,
		UseTLS:      cfg.ManagerTLS,
		CAFile:      cfg.ManagerTLSCA,
		ServerName:  cfg.ManagerTLSServerName,
		ProcessName: "frontend",
		ConfigPath:  frontendconfig.DefaultPath,
	})
	if err := checkManagerLink(context.Background(), link, managerCheckGrace, log.Printf); err != nil {
		return err
	}

	// The persisted role map (see internal/loginconfig, wired to the
	// Users page's Admin-only add/change-role/remove actions) is the
	// sole source of who may log in - physical, per-node state, never
	// routed through raft, since who may log in to one Hive's web UI is
	// that Hive's own concern. When no file has ever been written yet
	// (a fresh Comb), roleMap starts empty and the very first
	// successful login becomes Admin automatically (ADR-0086,
	// replacing the old -role-map bootstrap flag) - see
	// frontend.Server.bootstrapFirstAdmin.
	roleMap := map[string]manager.Role{}
	roleMapMgr := &loginconfig.Manager{}
	if cfg, exists, err := roleMapMgr.Load(); err != nil {
		return fmt.Errorf("loading persisted role map: %w", err)
	} else if exists {
		roleMap = make(map[string]manager.Role, len(cfg.RoleMap))
		for user, role := range cfg.RoleMap {
			roleMap[user] = manager.Role(role)
		}
	}

	// Whether login is enabled at all is now managerd's own call (ADR-
	// 0087, PamConfigured on its Status response) - frontend needs no
	// login-related flag of its own anymore. checkPAMConfigured retries
	// a few times (see its own doc comment for the exact startup race
	// this closes) before accepting its answer. An unavailable managerd
	// does not establish that login is disabled, so frontend fails closed
	// instead of serving an unauthenticated UI with an unknown policy.
	managerClient := rpcpb.NewManagerServiceClient(conn)
	auth, err := managerAuthenticator(managerClient, statusRetryAttempts, statusRetryDelay)
	if err != nil {
		return err
	}

	// Reuses the same API key already attached to dialOpts above for
	// this node's own managerd - a fetch of another node's HostStats
	// goes through the same authenticated ManagerService API (HostStats
	// requires only RoleViewer, well within what that key already
	// grants) rather than needing a second, separately-configured
	// credential.
	peers := manager.NewPeerReporter(cfg.ManagerAPIKey, cfg.PeerTLS, nil)
	pool, err := manager.ResolvePeerCAPool(cfg.PeerTLSCA)
	if err != nil {
		return fmt.Errorf("frontend: %w", err)
	}
	if pool != nil {
		peers.CAPool = pool
	}

	// Validated before NewServer, not after, so the session cookie's
	// Secure flag (set from tlsEnabled - see NewServer/handleLogin) is
	// never wrong for a moment: a cookie issued as non-Secure at
	// construction time could later cross a plaintext channel even if
	// serving is fixed to reject a mismatched cert/key pair afterward.
	tlsEnabled := cfg.TLSCert != "" || cfg.TLSKey != ""
	if tlsEnabled && (cfg.TLSCert == "" || cfg.TLSKey == "") {
		return fmt.Errorf("both tls_cert and tls_key must be set together")
	}

	srv, err := frontend.NewServer(managerClient, auth, roleMap, peers, cfg.PeerHostnameSuffix, cfg.PeerManagerPort, frontend.UnixPasswordSetter{}, tlsEnabled)
	if err != nil {
		return fmt.Errorf("creating frontend server: %w", err)
	}
	srv.SetRoleMapStore(roleMapMgr)
	// The host package page reads each Comb's inventory from that Comb's
	// own managerd (node-local, like HostStats) and needs nothing beyond
	// Viewer, so it is wired unconditionally rather than behind a flag:
	// the page's write half does not exist, so there is nothing to gate.
	srv.EnableHostPkgSource()
	if auth != nil {
		if len(roleMap) == 0 {
			log.Printf("frontend: login enabled (managerd reports PAM configured), no accounts yet - the first successful login becomes Admin")
		} else {
			log.Printf("frontend: login enabled (managerd reports PAM configured, %d role-mapped user(s))", len(roleMap))
		}
	} else {
		log.Printf("frontend: no login configured (set pam_service in managerd's own config to require one)")
	}

	log.Printf("frontend: %s listening on %s (manager-addr=%s, manager-tls=%v, tls=%v)", buildinfo.String(), cfg.HTTPAddr, cfg.ManagerAddr, cfg.ManagerTLS, tlsEnabled)
	httpSrv := httpserver.New(cfg.HTTPAddr, srv)
	if tlsEnabled {
		return httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	}
	return httpSrv.ListenAndServe()
}

// checkManagerLink turns the startup check's answer into a decision.
//
// A permanent failure - the two files disagree about the transport
// scheme, or the certificate does not verify - refuses to start. Serving
// anyway would mean serving 100% failures, and worse, a caller retrying a
// 500 that no retry can fix. Everything else (managerd not up yet, a PF
// rule in the way) is not evidence of a misconfiguration, so it starts
// degraded and says so loudly: the TLS setting is UNVERIFIED, every call
// will fail until managerd answers, and neither of those is a statement
// about whether the config is right.
func checkManagerLink(ctx context.Context, v verifier, grace time.Duration, logf func(string, ...any)) error {
	err := v.Verify(ctx, grace)
	if err == nil {
		return nil
	}
	if managerlink.IsPermanent(err) {
		return err
	}
	logf("frontend: WARNING: %v", err)
	logf("frontend: WARNING: starting in a DEGRADED state: every call to managerd will fail until it is " +
		"reachable, and the manager_tls setting is UNVERIFIED until then - this is not a statement that the " +
		"config is wrong, only that nothing has been able to check it")
	return nil
}
