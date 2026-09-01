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
	"strings"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/frontend"
	"github.com/glenjbarber/apiary/internal/manager"
	"github.com/glenjbarber/apiary/internal/pam"
	"github.com/glenjbarber/apiary/internal/tlsdial"
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

// parseRoleMap parses the -role-map flag's
// "admin:alice;operator:bob,carol;viewer:dave" format into a
// per-username Role lookup (ADR-0030) - deliberately independent of
// any UNIX/AD group, per the project's explicit "don't use the
// operator GID" requirement. An empty spec is valid (no login is
// possible until at least one username is mapped, since handleLogin
// rejects any username with no entry).
func parseRoleMap(spec string) (map[string]manager.Role, error) {
	roleMap := make(map[string]manager.Role)
	if strings.TrimSpace(spec) == "" {
		return roleMap, nil
	}
	for _, group := range strings.Split(spec, ";") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		parts := strings.SplitN(group, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid -role-map entry %q: want \"role:user1,user2\"", group)
		}
		role := manager.Role(strings.TrimSpace(parts[0]))
		switch role {
		case manager.RoleAdmin, manager.RoleOperator, manager.RoleViewer:
		default:
			return nil, fmt.Errorf("invalid -role-map role %q: want admin, operator, or viewer", role)
		}
		for _, user := range strings.Split(parts[1], ",") {
			user = strings.TrimSpace(user)
			if user == "" {
				continue
			}
			if existing, dup := roleMap[user]; dup {
				return nil, fmt.Errorf("-role-map lists user %q twice (as both %s and %s)", user, existing, role)
			}
			roleMap[user] = role
		}
	}
	return roleMap, nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("frontend: %v", err)
	}
}

func run() error {
	managerAddr := flag.String("manager-addr", "127.0.0.1:17700", "TCP address of managerd's external RPC API")
	httpAddr := flag.String("http-addr", "127.0.0.1:8080", "address to serve the web UI on")
	pamService := flag.String("pam-service", "", "PAM service name to authenticate web UI logins against (requires a matching /etc/pam.d/<name> on this host - ADR-0030); leave empty to disable login entirely")
	roleMapFlag := flag.String("role-map", "", "maps usernames to Apiary roles, e.g. \"admin:alice;operator:bob,carol;viewer:dave\" (ADR-0030) - a PAM login for a username with no entry here is rejected, not silently downgraded to viewer")
	managerTLS := flag.Bool("manager-tls", false, "dial managerd over TLS instead of plaintext (must match managerd's own -tls-cert/-tls-key)")
	managerTLSCA := flag.String("manager-tls-ca", "", "PEM CA file to trust for managerd's certificate (for a self-signed cert); leave empty to trust the system certificate pool")
	managerTLSServerName := flag.String("manager-tls-server-name", "", "hostname to verify managerd's certificate against, if different from -manager-addr's host (e.g. managerd stays loopback-only but its cert names a real public hostname); leave empty to verify against -manager-addr itself")
	tlsCert := flag.String("tls-cert", "", "PEM certificate file to serve the web UI over HTTPS; leave unset (with -tls-key) to serve plaintext HTTP, as before")
	tlsKey := flag.String("tls-key", "", "PEM private key file matching -tls-cert")
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

	roleMap, err := parseRoleMap(*roleMapFlag)
	if err != nil {
		return fmt.Errorf("parsing -role-map: %w", err)
	}

	var auth pam.Authenticator
	if *pamService != "" {
		auth = pam.PAMAuthenticator{ServiceName: *pamService}
	}

	srv, err := frontend.NewServer(rpcpb.NewManagerServiceClient(conn), auth, roleMap)
	if err != nil {
		return fmt.Errorf("creating frontend server: %w", err)
	}
	if auth != nil {
		log.Printf("frontend: login enabled (pam-service=%s, %d role-mapped user(s))", *pamService, len(roleMap))
	} else {
		log.Printf("frontend: no login configured (set -pam-service/-role-map to require one)")
	}

	log.Printf("frontend: listening on %s (manager-addr=%s, manager-tls=%v, tls=%v)", *httpAddr, *managerAddr, *managerTLS, *tlsCert != "")
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			return fmt.Errorf("both -tls-cert and -tls-key must be set together")
		}
		return http.ListenAndServeTLS(*httpAddr, *tlsCert, *tlsKey, srv)
	}
	return http.ListenAndServe(*httpAddr, srv)
}
