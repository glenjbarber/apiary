// Command frontend serves Apiary's HTMX web UI. It is a client of
// managerd's external gRPC API (api/rpc.ManagerService), the same way
// internal/restshim is - it never talks to raftd directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/frontend"
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
	managerAddr := flag.String("manager-addr", "127.0.0.1:17700", "TCP address of managerd's external RPC API")
	httpAddr := flag.String("http-addr", "127.0.0.1:8080", "address to serve the web UI on")
	managerTLS := flag.Bool("manager-tls", false, "dial managerd over TLS instead of plaintext (must match managerd's own -tls-cert/-tls-key)")
	managerTLSCA := flag.String("manager-tls-ca", "", "PEM CA file to trust for managerd's certificate (for a self-signed cert); leave empty to trust the system certificate pool")
	managerTLSServerName := flag.String("manager-tls-server-name", "", "hostname to verify managerd's certificate against, if different from -manager-addr's host (e.g. managerd stays loopback-only but its cert names a real public hostname); leave empty to verify against -manager-addr itself")
	tlsCert := flag.String("tls-cert", "", "PEM certificate file to serve the web UI over HTTPS; leave unset (with -tls-key) to serve plaintext HTTP, as before")
	tlsKey := flag.String("tls-key", "", "PEM private key file matching -tls-cert")
	peerTLS := flag.Bool("peer-tls", false, "dial other cluster nodes' managerd over TLS when fetching their host stats for the cluster overview page; requires each peer's managerd to also be TLS-enabled with a certificate matching its own hostname (node ID + -peer-hostname-suffix)")
	peerHostnameSuffix := flag.String("peer-hostname-suffix", "", "appended to a node ID to form its managerd hostname for the cluster overview page (e.g. \".apiary.work\" so node ID \"freebsd-apiary\" dials \"freebsd-apiary.apiary.work\"); leave empty if node IDs are already fully-qualified hostnames")
	peerManagerPort := flag.String("peer-manager-port", "17700", "port assumed for a peer node's managerd external API when fetching its host stats")
	flag.Parse()

	managerCreds, err := tlsdial.ManagerDialOption(*managerTLS, *managerTLSCA, *managerTLSServerName)
	if err != nil {
		return err
	}
	dialOpts := []grpc.DialOption{managerCreds}
	// APIARY_MANAGER_API_KEY authenticates every call to managerd once
	// it has API-key auth enabled (ADR-0023) - unset by default, since
	// auth is opt-in and off until the first key is ever created via
	// the /apikeys page. Once that happens, this must be set (and
	// frontend restarted) or every call starts failing Unauthenticated.
	// The key's own role (ADR-0030) must be at least Operator, since
	// frontend forwards whatever the logged-in user's role permits -
	// a frontend whose own key is only Viewer would reject every
	// Operator/Admin action downstream regardless of the UI session.
	if apiKey := os.Getenv("APIARY_MANAGER_API_KEY"); apiKey != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(apiKeyCredentials(apiKey)))
	}
	conn, err := grpc.NewClient(*managerAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("dialing managerd at %s: %w", *managerAddr, err)
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
	peers := manager.NewPeerReporter(os.Getenv("APIARY_MANAGER_API_KEY"), *peerTLS, nil)

	// Validated before NewServer, not after, so the session cookie's
	// Secure flag (set from tlsEnabled - see NewServer/handleLogin) is
	// never wrong for a moment: a cookie issued as non-Secure at
	// construction time could later cross a plaintext channel even if
	// serving is fixed to reject a mismatched cert/key pair afterward.
	tlsEnabled := *tlsCert != "" || *tlsKey != ""
	if tlsEnabled && (*tlsCert == "" || *tlsKey == "") {
		return fmt.Errorf("both -tls-cert and -tls-key must be set together")
	}

	srv, err := frontend.NewServer(managerClient, auth, roleMap, peers, *peerHostnameSuffix, *peerManagerPort, frontend.UnixPasswordSetter{}, tlsEnabled)
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
		log.Printf("frontend: no login configured (set -pam-service on managerd to require one)")
	}

	log.Printf("frontend: listening on %s (manager-addr=%s, manager-tls=%v, tls=%v)", *httpAddr, *managerAddr, *managerTLS, tlsEnabled)
	if tlsEnabled {
		return http.ListenAndServeTLS(*httpAddr, *tlsCert, *tlsKey, srv)
	}
	return http.ListenAndServe(*httpAddr, srv)
}
