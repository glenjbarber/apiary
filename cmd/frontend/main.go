// Command frontend serves Apiary's HTMX web UI. It is a client of
// managerd's external gRPC API (api/rpc.ManagerService), the same way
// internal/restshim is - it never talks to raftd directly.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/frontend"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/loginconfig"
	"github.com/glenjbarber/apiary/internal/manager"
	"github.com/glenjbarber/apiary/internal/tlsdial"
)

// statusRetryAttempts/statusRetryDelay bound how long checkPAMConfigured
// waits for managerd to become reachable at frontend's own startup - see
// that function's own doc comment for why this exists at all.
const (
	statusRetryAttempts = 5
	statusRetryDelay    = time.Second
)

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
// per-request. Still falls back to "unreachable" once attempts are
// exhausted, preserving the existing policy that frontend always
// finishes starting up regardless of managerd's state.
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

func main() {
	if err := run(); err != nil {
		log.Fatalf("frontend: %v", err)
	}
}

func run() error {
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
	// this closes) before falling back to "not configured" - frontend
	// should still come up and serve pages even if managerd stays
	// genuinely unreachable well past that.
	managerClient := rpcpb.NewManagerServiceClient(conn)
	var auth frontend.Authenticator
	if configured, err := checkPAMConfigured(managerClient, statusRetryAttempts, statusRetryDelay); err != nil {
		log.Printf("frontend: could not reach managerd to check login configuration after %d attempts: %v", statusRetryAttempts, err)
	} else if configured {
		auth = remoteAuthenticator{client: managerClient}
	}

	// Reuses the same API key already attached to dialOpts above for
	// this node's own managerd - a fetch of another node's HostStats
	// goes through the same authenticated ManagerService API (HostStats
	// requires only RoleViewer, well within what that key already
	// grants) rather than needing a second, separately-configured
	// credential.
	peers := manager.NewPeerReporter(cfg.ManagerAPIKey, cfg.PeerTLS, nil)
	if cfg.PeerTLSCA != "" {
		pool, err := manager.LoadPeerCAPool(cfg.PeerTLSCA)
		if err != nil {
			return fmt.Errorf("frontend: %w", err)
		}
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
	if auth != nil {
		if len(roleMap) == 0 {
			log.Printf("frontend: login enabled (managerd reports PAM configured), no accounts yet - the first successful login becomes Admin")
		} else {
			log.Printf("frontend: login enabled (managerd reports PAM configured, %d role-mapped user(s))", len(roleMap))
		}
	} else {
		log.Printf("frontend: no login configured (set pam_service in managerd's own config to require one)")
	}

	log.Printf("frontend: listening on %s (manager-addr=%s, manager-tls=%v, tls=%v)", cfg.HTTPAddr, cfg.ManagerAddr, cfg.ManagerTLS, tlsEnabled)
	if tlsEnabled {
		return http.ListenAndServeTLS(cfg.HTTPAddr, cfg.TLSCert, cfg.TLSKey, srv)
	}
	return http.ListenAndServe(cfg.HTTPAddr, srv)
}
