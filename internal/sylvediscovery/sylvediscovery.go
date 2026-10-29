// Package sylvediscovery implements the read-only Sylve discovery
// provider described by ADR-0192.
//
// It is a PROVIDER, not a control plane: every request this package
// makes is a GET to one of a fixed set of paths, and there is no way
// from this package's API to create, modify, or remove anything in
// Sylve. Sylve remains authoritative for its own state; this package
// only observes it.
//
// Three Sylve behaviours shape this implementation, all verified
// against upstream source rather than assumed:
//
//   - THE ENVELOPE. Sylve's endpoints return
//     {"status":"success","message":"...","error":"","data":...}. An
//     ERROR response has the same shape with "status":"error" and a
//     null "data". A client that reads "data" without checking
//     "status" sees null and may render it as "no guests". So the
//     status is checked FIRST, always, and an error envelope is a
//     failure rather than an empty inventory.
//
//   - THE PARTIAL FLAG. GET /cluster's response carries a "partial"
//     indicator, and GET /cluster/resources can fail for one node
//     while succeeding for others. A partial response is not a
//     complete one, and treating it as complete is how a discovery
//     integration concludes that absent guests were deleted.
//
//   - THE CREDENTIAL. Sylve's authorization model refuses a non-admin
//     identity on every mutating route (RequireLocalAdmin /
//     RequireLocalAdminForWrites), while the discovery routes carry
//     only EnsureAuthenticated. So a non-admin token is safe. But
//     Sylve's LOGIN flow cannot mint one - CreateJWT refuses a
//     non-admin "sylve" login - so the credential must be provisioned
//     out of band. This package therefore never performs a login; it
//     only presents a token that was provisioned for it.
//
// The endpoint paths are compile-time constants. The allowlist is not
// configuration, so it cannot be widened by editing a config file.
package sylvediscovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/extdiscovery"
)

// ProviderName is this provider's short identifier. It is part of
// every record's composite identity.
const ProviderName = "sylve"

// allowedEndpoints is the complete set of paths this provider may
// request.
//
// It is a constant, not configuration, for the same reason Proxmox's
// is: a configurable allowlist is one an operator can widen, and
// widening it past these read paths is how a read-only provider grows
// a write path.
//
// Note what is ABSENT: there is no "/events/stream". SSE takes its
// token as a query parameter, so it lands in access logs and proxy
// logs. Polling is the correctness mechanism and SSE is not
// implemented - see ADR-0192's "Polling, not SSE".
var allowedEndpoints = []string{
	"/cluster",
	"/cluster/nodes",
	"/cluster/resources",
	"/vm/simple",
	"/jail/simple",
}

// The fixed paths.
const (
	clusterPath          = "/cluster"
	clusterNodesPath     = "/cluster/nodes"
	clusterResourcesPath = "/cluster/resources"
	vmSimplePath         = "/vm/simple"
	jailSimplePath       = "/jail/simple"
)

// Config configures a Sylve discovery provider.
type Config struct {
	// BaseURL is the Sylve API origin including the "/api" base path,
	// e.g. "https://sylve.example.com:8181/api".
	BaseURL string

	// CAFile is an optional path to a PEM bundle of trusted roots. When
	// empty, the system trust store is used.
	//
	// There is no insecure-skip-verify option. A Sylve installation
	// using a private CA is supported by pointing this at its bundle.
	CAFile string

	// TokenFile is the path to the file holding the bearer token.
	//
	// The token must belong to a NON-ADMIN Sylve identity, and it must
	// be provisioned out of band: Sylve's login endpoint refuses to
	// mint a token for a non-admin account, and using an
	// administrator's token would make the credential write-capable
	// even though this package only issues GETs.
	//
	// The file is read fresh on each scan, so rotation takes effect
	// without a restart.
	TokenFile string

	// Timeout bounds one request. Zero means the shared default.
	Timeout time.Duration
}

// Provider is the Sylve discovery provider.
type Provider struct {
	client *extdiscovery.HTTPClient
	cfg    Config
}

// New builds a Sylve discovery provider.
func New(cfg Config) (*Provider, error) {
	client, err := extdiscovery.NewHTTPClient(cfg.BaseURL, cfg.CAFile)
	if err != nil {
		return nil, err
	}
	if cfg.Timeout > 0 {
		client.Timeout = cfg.Timeout
	}
	if strings.TrimSpace(cfg.TokenFile) == "" {
		return nil, fmt.Errorf("sylve discovery requires a token file")
	}
	return &Provider{client: client, cfg: cfg}, nil
}

// Name implements extdiscovery.Provider.
func (p *Provider) Name() string { return ProviderName }

// envelope is Sylve's API response wrapper.
//
// The status/message/error fields are checked before data is read. An
// error response carries a null data field, and a client that reads
// data directly would see nil and report it as an empty inventory.
type envelope struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// clusterResponse is GET /cluster's payload.
//
// The Partial field is the single most important field here: a partial
// response is not a complete one.
type clusterResponse struct {
	ClusterID string `json:"clusterId"`
	Enabled   bool   `json:"enabled"`
	NodeID    string `json:"nodeId"`
	Leader    string `json:"leader"`
	Members   []any  `json:"members"`
	Guests    []any  `json:"guests"`
	Partial   bool   `json:"partial"`
}

// clusterNodeResponse is GET /cluster/nodes's payload.
type clusterNodeResponse struct {
	UUID     string `json:"uuid"`
	Hostname string `json:"hostname"`
	Status   string `json:"status"`
	// The capacity and usage figures are optional and are decoded
	// loosely; a Sylve version that does not report them is not an
	// error.
	CPUCapacity    float64 `json:"cpuCapacity"`
	MemoryCapacity int64   `json:"memoryCapacity"`
	DiskCapacity   int64   `json:"diskCapacity"`
}

// nodeResourcesResponse is GET /cluster/resources's payload: the guests
// grouped by node.
//
// The per-node grouping and the nodeUUID association are preserved,
// because a resource's node is part of its composite identity and
// losing it would make the record unplaceable.
type nodeResourcesResponse struct {
	Hostname string  `json:"hostname"`
	NodeUUID string  `json:"nodeUUID"`
	VMs      []guest `json:"vms"`
	Jails    []guest `json:"jails"`
}

// guest is one VM or jail within a node's resource list.
type guest struct {
	ID    string `json:"id"`
	RID   int    `json:"rid"`
	CTID  int    `json:"ctid"`
	Name  string `json:"name"`
	State string `json:"state"`
	// Cores and memory are optional.
	Cores  int   `json:"cores"`
	Memory int64 `json:"memory"`
}

// Scan implements extdiscovery.Provider.
//
// It performs one discovery scan and reports what it saw AND how
// complete the scan was.
//
// The completeness rules here are the substance of ADR-0192:
//
//   - A failure at any step means the scan is INCOMPLETE and the error
//     is reported. The caller's store preserves the last known records
//     and marks them stale.
//
//   - A "partial" cluster response is PARTIAL, whatever else it
//     contained.
//
//   - A scan that observed nothing, having succeeded, is EMPTY - which
//     is a fact, not a failure.
func (p *Provider) Scan(ctx context.Context) extdiscovery.ScanResult {
	now := time.Now().UTC()
	result := extdiscovery.ScanResult{ObservedAt: now}

	// Step 1: cluster identity and the partial indicator.
	var clusterEnv envelope
	if err := p.client.GetJSON(ctx, allowedEndpoints, clusterPath, p.cfg.TokenFile, &clusterEnv); err != nil {
		result.State = extdiscovery.StateFor(err)
		result.Err = err
		return result
	}
	if err := checkEnvelope(clusterEnv, clusterPath); err != nil {
		result.State = extdiscovery.StateFor(err)
		result.Err = err
		return result
	}

	var cluster clusterResponse
	if len(clusterEnv.Data) > 0 {
		if err := json.Unmarshal(clusterEnv.Data, &cluster); err != nil {
			result.State = extdiscovery.StateUnavailable
			result.Err = &extdiscovery.ProviderError{Endpoint: clusterPath, Err: fmt.Errorf("decoding cluster payload: %w", err)}
			return result
		}
	}

	sourceID := strings.TrimSpace(cluster.ClusterID)
	result.SourceID = sourceID

	// Step 2: the node list.
	var nodesEnv envelope
	if err := p.client.GetJSON(ctx, allowedEndpoints, clusterNodesPath, p.cfg.TokenFile, &nodesEnv); err != nil {
		result.State = extdiscovery.StateFor(err)
		result.Err = err
		return result
	}
	if err := checkEnvelope(nodesEnv, clusterNodesPath); err != nil {
		result.State = extdiscovery.StateFor(err)
		result.Err = err
		return result
	}

	var nodes []clusterNodeResponse
	if len(nodesEnv.Data) > 0 {
		if err := json.Unmarshal(nodesEnv.Data, &nodes); err != nil {
			result.State = extdiscovery.StateUnavailable
			result.Err = &extdiscovery.ProviderError{Endpoint: clusterNodesPath, Err: fmt.Errorf("decoding node payload: %w", err)}
			return result
		}
	}

	observed := make([]extdiscovery.Node, 0, len(nodes))
	for _, n := range nodes {
		name := strings.TrimSpace(n.Hostname)
		if name == "" {
			name = strings.TrimSpace(n.UUID)
		}
		if name == "" {
			// A node with neither a hostname nor a UUID cannot be
			// identified, so it cannot be recorded.
			continue
		}
		observed = append(observed, extdiscovery.Node{
			Provider:    ProviderName,
			SourceID:    sourceID,
			Name:        name,
			Fingerprint: strings.TrimSpace(n.UUID),
			Status:      n.Status,
			ObservedAt:  now,
		})
	}
	result.Nodes = observed

	// Step 3: the cluster-wide guest inventory.
	var resEnv envelope
	if err := p.client.GetJSON(ctx, allowedEndpoints, clusterResourcesPath, p.cfg.TokenFile, &resEnv); err != nil {
		result.State = extdiscovery.StateFor(err)
		result.Err = err
		return result
	}
	if err := checkEnvelope(resEnv, clusterResourcesPath); err != nil {
		result.State = extdiscovery.StateFor(err)
		result.Err = err
		return result
	}

	var byNode []nodeResourcesResponse
	if len(resEnv.Data) > 0 {
		if err := json.Unmarshal(resEnv.Data, &byNode); err != nil {
			result.State = extdiscovery.StateUnavailable
			result.Err = &extdiscovery.ProviderError{Endpoint: clusterResourcesPath, Err: fmt.Errorf("decoding resource payload: %w", err)}
			return result
		}
	}

	resources := make([]extdiscovery.Resource, 0)
	for _, node := range byNode {
		nodeName := strings.TrimSpace(node.Hostname)
		nodeID := strings.TrimSpace(node.NodeUUID)
		if nodeName == "" && nodeID == "" {
			continue
		}
		for _, vm := range node.VMs {
			id := resourceID(vm.RID, vm.ID)
			if id == "" {
				continue
			}
			resources = append(resources, extdiscovery.Resource{
				Provider:         ProviderName,
				SourceID:         sourceID,
				NodeName:         nodeName,
				NodeID:           nodeID,
				Kind:             extdiscovery.KindVM,
				SourceResourceID: id,
				Name:             vm.Name,
				State:            vm.State,
				ObservedAt:       now,
			})
		}
		for _, jail := range node.Jails {
			id := resourceID(jail.CTID, jail.ID)
			if id == "" {
				continue
			}
			resources = append(resources, extdiscovery.Resource{
				Provider:         ProviderName,
				SourceID:         sourceID,
				NodeName:         nodeName,
				NodeID:           nodeID,
				Kind:             extdiscovery.KindJail,
				SourceResourceID: id,
				Name:             jail.Name,
				State:            jail.State,
				ObservedAt:       now,
			})
		}
	}
	result.Resources = resources

	// A partial cluster response is not a complete scan, whatever it
	// contained. Absence in a partial scan is UNKNOWN, not absent.
	if cluster.Partial {
		result.State = extdiscovery.StatePartial
		result.Complete = false
		return result
	}

	if len(observed) == 0 && len(resources) == 0 {
		result.State = extdiscovery.StateEmpty
		result.Complete = true
		return result
	}

	result.State = extdiscovery.StateFresh
	result.Complete = true
	return result
}

// checkEnvelope verifies a response envelope's status before its data
// is read.
//
// The check is the reason an error response is never mistaken for an
// empty inventory. Sylve's error response has the same shape as a
// success response with a null data field, so a client that read data
// alone would see "nothing there" where the truth is "the request
// failed".
func checkEnvelope(env envelope, endpoint string) error {
	switch strings.ToLower(strings.TrimSpace(env.Status)) {
	case "success", "":
		return nil
	case "error":
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = "provider reported an error"
		}
		return &extdiscovery.ProviderError{
			Endpoint: endpoint,
			Err:      fmt.Errorf("%s", msg),
		}
	default:
		return &extdiscovery.ProviderError{
			Endpoint: endpoint,
			Err:      fmt.Errorf("provider returned an unrecognised status %q", env.Status),
		}
	}
}

// resourceID picks a resource's stable source identifier.
//
// A VM's RID and a jail's CTID are the provider's own numeric
// identifiers. The string ID is used as a fallback when the numeric one
// is absent, because a record with no identifier at all cannot be
// placed.
//
// The two are kept in different namespaces by the composite key's Kind
// component, so a VM with RID 100 and a jail with CTID 100 do not
// collide.
func resourceID(numeric int, id string) string {
	if numeric > 0 {
		return fmt.Sprintf("%d", numeric)
	}
	return strings.TrimSpace(id)
}

// urlPathEscape is used by callers that build a per-resource path. It
// is defined here so the provider's path-building is uniform.
func urlPathEscape(s string) string { return url.PathEscape(s) }
