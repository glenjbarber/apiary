package frontend

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/glenjbarber/apiary/internal/manager"
)

const restshimAddress = "http://127.0.0.1:8081"

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

// NewAPIProxy creates the optional proxy to restshimd's fixed loopback
// listener. The route table intentionally mirrors restshimd's registered
// methods, while the upstream keeps the /v1 prefix after frontend strips /api.
func NewAPIProxy() http.Handler {
	return newAPIProxy(restshimAddress)
}

func newAPIProxy(upstream string) http.Handler {
	target, err := url.Parse(upstream)
	if err != nil {
		panic("invalid restshimd proxy target: " + err.Error())
	}
	proxy := &httputil.ReverseProxy{
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
	return mux
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
