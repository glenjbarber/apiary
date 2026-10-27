package frontend

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/glenjbarber/apiary/internal/manager"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
)

// restshimDefaultAddress is restshimd's default loopback address, and
// the address its own config uses when it says nothing else.
const restshimDefaultAddress = "127.0.0.1:8081"

// apiProxyRoute is an explicit frontend allow-list. Keep this aligned with
// internal/restshim/server.go:routes; adding a restshimd route must not expose
// it through frontend by accident.
type apiProxyRoute struct {
	pattern   string
	rpcMethod string
}

var apiProxyRoutes = []apiProxyRoute{
	{"GET /api/v1/status", "Status"},
	{"GET /api/v1/health", "ClusterHealth"},
	{"GET /api/v1/monitoring/health", "GetMonitoringCluster"},
	{"GET /api/v1/monitoring/nodes", "GetMonitoringNodes"},
	{"POST /api/v1/vms", "CreateVM"},
	{"GET /api/v1/vms", "ListVMs"},
	{"GET /api/v1/vms/{id}", "GetVM"},
	{"PUT /api/v1/vms/{id}", "UpdateVM"},
	{"DELETE /api/v1/vms/{id}", "DeleteVM"},
	{"POST /api/v1/vms/{id}/migrate", "MigrateVM"},
	{"POST /api/v1/cells", "CreateCell"},
	{"POST /api/v1/cells/recommend", "RecommendCellType"},
	{"POST /api/v1/jails", "CreateJail"},
	{"GET /api/v1/jails", "ListJails"},
	{"GET /api/v1/jails/{id}", "GetJail"},
	{"PUT /api/v1/jails/{id}", "UpdateJail"},
	{"DELETE /api/v1/jails/{id}", "DeleteJail"},
	{"POST /api/v1/jails/{id}/migrate", "MigrateJail"},
	{"POST /api/v1/networks", "CreateNetwork"},
	{"GET /api/v1/networks", "ListNetworks"},
	{"DELETE /api/v1/networks/{id}", "DeleteNetwork"},
	{"POST /api/v1/isos", "UploadISO"},
}

// restshimdUpstream describes how to reach restshimd's loopback listener,
// as restshimd's OWN config says it listens.
//
// Reading that config is the same posture ADR-0102 already established
// for the sibling daemons: managerd reads frontend.json, restshimd.json
// and raftd.json on the same host, because they are co-located and each
// owns exactly one file. frontend is the fourth sibling reading the
// fifth file, for the same reason - the address and scheme it needs are
// restshimd's to decide, not frontend's to assume.
type restshimdUpstream struct {
	// address is the host:port restshimd listens on.
	address string
	// useTLS is whether restshimd serves HTTPS on that address.
	useTLS bool
	// caFile is a PEM bundle to trust for restshimd's certificate,
	// instead of the system pool. Empty means the system pool.
	caFile string
	// serverName overrides the name verified against restshimd's
	// certificate, independently of the address dialed.
	serverName string
}

// restshimdUpstreamFromConfig reads restshimd's own config file to learn
// how its listener is set up.
//
// A config that cannot be read or parsed yields restshimd's OWN defaults
// rather than an error: a missing restshimd.json means restshimd is
// running with its built-in defaults, and a proxy that assumed the same
// defaults is at least consistent with the daemon it is proxying to.
// Failing to start frontend because a sibling's file is absent would be
// a worse outcome than the failure it prevents.
//
// The one case that IS an error is a config that is present and usable
// but names a certificate this proxy cannot load - there is no consistent
// reading of that, and guessing would mean failing on the first proxied
// request instead of at startup.
func restshimdUpstreamFromConfig() (restshimdUpstream, error) {
	upstream := restshimdUpstream{address: restshimDefaultAddress}
	cfg, err := (&restshimdconfig.Manager{}).Load()
	if err != nil {
		// Not "no such file" - a config that exists but cannot be
		// parsed is an operator-visible problem, not a default.
		if _, statErr := os.Stat(restshimdconfig.DefaultPath); statErr == nil {
			return upstream, fmt.Errorf("reading restshimd config %s: %w", restshimdconfig.DefaultPath, err)
		}
		return upstream, nil
	}
	if addr := strings.TrimSpace(cfg.HTTPAddr); addr != "" {
		upstream.address = addr
	}
	// cmd/restshimd serves TLS when it has BOTH halves of a pair, and
	// refuses to start with only one, so a half-configured pair means it
	// is not serving TLS at all.
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		upstream.useTLS = true
		upstream.caFile = cfg.TLSCert
	}
	return upstream, nil
}

// NewAPIProxy creates the optional proxy to restshimd's loopback
// listener. The route table intentionally mirrors restshimd's registered
// methods, while the upstream keeps the /v1 prefix after frontend strips
// /api.
//
// restshimd's own config decides whether that listener speaks HTTP or
// HTTPS: cmd/restshimd serves TLS whenever its config sets
// tls_cert/tls_key, and the shipped restshimd.json.sample sets both. A
// proxy that assumed plaintext would fail every request against such an
// installation - restshimd answers "Client sent an HTTP request to an
// HTTPS server" - so the scheme is resolved from restshimd's own config
// rather than assumed.
func NewAPIProxy() (http.Handler, error) {
	upstream, err := restshimdUpstreamFromConfig()
	if err != nil {
		return nil, err
	}
	return newAPIProxy(upstream)
}

// newAPIProxy builds the proxy for one upstream.
func newAPIProxy(upstream restshimdUpstream) (http.Handler, error) {
	scheme := "http"
	transport := http.DefaultTransport
	if upstream.useTLS {
		scheme = "https"
		tlsConfig, err := restshimdTLSConfig(upstream)
		if err != nil {
			// A certificate that cannot be loaded is a startup
			// problem, not a runtime surprise on the first proxied
			// request - so it is reported, not guessed at.
			return nil, err
		}
		transport = &http.Transport{TLSClientConfig: tlsConfig}
	}
	address := upstream.address
	// An address that already carries a scheme is taken as one - a
	// test harness passes a full URL this way. restshimd's own config
	// never does: its http_addr is a bare host:port.
	if parsed, err := url.Parse(address); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		if parsed.Scheme == "https" {
			transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
		}
		scheme = parsed.Scheme
		address = parsed.Host
	}
	target := &url.URL{Scheme: scheme, Host: address}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(r *http.Request) {
			originalPath, originalRawPath := r.URL.Path, r.URL.RawPath
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			r.Host = target.Host
			r.URL.Path = strings.TrimPrefix(originalPath, "/api")
			if originalRawPath != "" {
				r.URL.RawPath = strings.TrimPrefix(originalRawPath, "/api")
			}
			r.Header.Del("Cookie")
		},
	}
	mux := http.NewServeMux()
	for _, route := range apiProxyRoutes {
		mux.Handle(route.pattern, proxy)
	}
	return mux, nil
}

// restshimdTLSConfig builds the client TLS configuration for the proxy's
// upstream dial. It mirrors internal/tlsdial's own rules - trust the
// system pool unless a CA file is named, verify against the dialed address
// unless a server name is given - because it is the same job: reaching a
// sibling daemon whose certificate this project issued itself.
func restshimdTLSConfig(upstream restshimdUpstream) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: upstream.serverName, MinVersion: tls.VersionTLS12}
	if upstream.caFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(upstream.caFile)
	if err != nil {
		return nil, fmt.Errorf("reading restshimd certificate %s: %w", upstream.caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no valid certificates found in %s", upstream.caFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

func hasBearerAuthorization(value string) bool {
	parts := strings.Fields(value)
	return len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && parts[1] != ""
}

// hasAuthorizationPolicy reports whether an RPC has a reviewed authorization
// rule. Status is the deliberate auth-exempt exception.
func hasAuthorizationPolicy(rpcMethod string) bool {
	if rpcMethod == "Status" {
		return true
	}
	return manager.HasExplicitRoleRequirement("/apiary.rpc.v1.ManagerService/" + rpcMethod)
}
