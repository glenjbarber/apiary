// Package proxmoxdiscovery implements the read-only Proxmox discovery
// provider described by ADR-0191.
//
// It is a PROVIDER, not a control plane: every request this package
// makes is a GET to one of three fixed paths, and there is no way from
// this package's API to create, modify, or remove anything in Proxmox.
// Proxmox remains authoritative for its cluster state; this package only
// observes it.
//
// The three endpoints, and the one behaviour of each that matters:
//
//   - GET /nodes is the cluster-wide node index. It is
//     permissions => { user => 'all' }, so any authenticated identity
//     may call it. IMPORTANT: it FILTERS rather than errors. Without
//     Sys.Audit on /nodes/{node}, the call still succeeds and the
//     per-node statistics (cpu, mem, maxcpu, maxmem, uptime) are simply
//     ABSENT from the response. A client that reads an absent field as
//     "this host has no memory" is wrong; this package therefore
//     treats those fields as optional and never infers a value from
//     their absence.
//
//   - GET /cluster/status carries the cluster identity - the
//     type:'cluster' row's name, nodes, version, and quorate. It
//     requires Sys.Audit on '/'. IMPORTANT: on a STANDALONE Proxmox
//     node there is no type:'cluster' row at all - the response is a
//     single synthetic type:'node' entry for the local node. A
//     standalone install is the common case, not an exotic one, so the
//     absence of a cluster name is represented as an empty SourceID
//     rather than being papered over.
//
//   - GET /nodes/{node}/status is per-node detail, proxyto'd to that
//     node, and requires Sys.Audit on /nodes/{node}. It carries
//     boot-info.mode, which is how Apiary tells a UEFI node from a
//     legacy-BIOS one.
//
// The endpoint paths are compile-time constants. The allowlist is not
// configuration, so it cannot be widened by editing a config file.
package proxmoxdiscovery

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/extdiscovery"
)

// ProviderName is this provider's short identifier. It is part of
// every record's composite identity.
const ProviderName = "proxmox"

// allowedEndpoints is the complete set of paths this provider may
// request.
//
// It is a constant, not configuration. That is deliberate: a
// configurable endpoint allowlist is an allowlist an operator can
// widen, and widening it past these three paths is how a read-only
// provider grows a write path.
var allowedEndpoints = []string{
	"/cluster/status",
	"/nodes",
	"/nodes/{param}/status",
}

// nodeListPath and clusterStatusPath are the two fixed paths.
const (
	nodeListPath      = "/nodes"
	clusterStatusPath = "/cluster/status"
)

// nodeStatusPath builds the per-node status path. The node name is
// URL-escaped, because a node name comes from an external system and
// must not be able to alter the request's shape.
func nodeStatusPath(node string) string {
	return "/nodes/" + url.PathEscape(node) + "/status"
}

// Config configures a Proxmox discovery provider.
type Config struct {
	// BaseURL is the pveproxy HTTPS origin, e.g.
	// "https://pve.example.com:8006".
	BaseURL string

	// CAFile is an optional path to a PEM bundle of trusted roots. When
	// empty, the system trust store is used.
	//
	// There is no insecure-skip-verify option. A Proxmox
	// installation using a private CA is supported by pointing this
	// at its bundle.
	CAFile string

	// TokenFile is the path to the file holding the API token. The
	// file is read fresh on each scan, so rotation takes effect
	// without a restart.
	//
	// The token is a Proxmox API token, which carries a privilege set
	// decided by the operator. The MINIMUM that works is decided by
	// which endpoints are called, and ADR-0191 records the open
	// question: Sys.Audit on '/' for GET /cluster/status, or only
	// per-node Sys.Audit if the operator chooses to give up the
	// cluster name.
	TokenFile string

	// Timeout bounds one request. Zero means the shared default.
	Timeout time.Duration
}

// Provider is the Proxmox discovery provider.
type Provider struct {
	client *extdiscovery.HTTPClient
	cfg    Config
}

// New builds a Proxmox discovery provider.
func New(cfg Config) (*Provider, error) {
	client, err := extdiscovery.NewHTTPClient(cfg.BaseURL, cfg.CAFile)
	if err != nil {
		return nil, err
	}
	if cfg.Timeout > 0 {
		client.Timeout = cfg.Timeout
	}
	if strings.TrimSpace(cfg.TokenFile) == "" {
		return nil, fmt.Errorf("proxmox discovery requires a token file")
	}
	return &Provider{client: client, cfg: cfg}, nil
}

// Name implements extdiscovery.Provider.
func (p *Provider) Name() string { return ProviderName }

// clusterStatusResponse is GET /cluster/status's response.
//
// It is an array of heterogeneous rows, each tagged with a "type" of
// either "cluster" or "node". A cluster row carries the cluster name;
// a node row carries a node's raft membership details.
//
// The rows are modelled loosely on purpose: the endpoint's schema
// documents only some of its fields, and an unmodelled field in the
// response is not an error - it is simply not used.
type clusterStatusResponse []clusterStatusRow

type clusterStatusRow struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Nodes   int    `json:"nodes"`
	Version int    `json:"version"`
	Quorate *bool  `json:"quorate"`
	NodeID  int    `json:"nodeid"`
	IP      string `json:"ip"`
	Local   bool   `json:"local"`
	Online  *bool  `json:"online"`
}

// nodeListResponse is GET /nodes's response.
type nodeListResponse []nodeListEntry

type nodeListEntry struct {
	Node string `json:"node"`
	// Type is "node" on every entry in practice; it is decoded so a
	// future row type is at least visible.
	Type string `json:"type"`
	// Status is "unknown", "online", or "offline".
	//
	// The values are kept VERBATIM. An unrecognised status is
	// preserved as-is rather than mapped, because mapping an unknown
	// value onto "offline" would assert something the source did not
	// say.
	Status string `json:"status"`
	// CPU and the rest are ABSENT when the credential lacks
	// Sys.Audit on /nodes/{node}. They are pointers so that absence
	// is distinguishable from a zero value.
	CPU    *float64 `json:"cpu"`
	MaxCPU *int     `json:"maxcpu"`
	Mem    *int64   `json:"mem"`
	MaxMem *int64   `json:"maxmem"`
	Uptime *int64   `json:"uptime"`
	Level  string   `json:"level"`
	// SSLFingerprint is the node's stable per-node identity anchor.
	SSLFingerprint string `json:"ssl_fingerprint"`
}

// nodeStatusResponse is GET /nodes/{node}/status's response.
//
// Only boot-info.mode is used. The endpoint's schema documents "the
// remaining ones" as unmodelled, so this struct models only what is
// read.
type nodeStatusResponse struct {
	BootInfo struct {
		Mode string `json:"mode"`
	} `json:"boot-info"`
}

// Scan implements extdiscovery.Provider.
//
// It performs one discovery scan and reports what it saw AND how
// complete the scan was.
//
// The completeness rules here are the substance of ADR-0191:
//
//   - A failure at any step means the scan is INCOMPLETE and the
//     error is reported. The caller's store preserves the last known
//     records and marks them stale.
//
//   - A scan that succeeded but could not read one node's detail is
//     PARTIAL: what it did observe is current, and what it did not
//     observe is unknown rather than absent.
//
//   - A scan that observed no nodes at all, having succeeded, is
//     EMPTY - which is a fact, not a failure.
func (p *Provider) Scan(ctx context.Context) extdiscovery.ScanResult {
	now := time.Now().UTC()
	result := extdiscovery.ScanResult{ObservedAt: now}

	// Step 1: cluster identity.
	var status clusterStatusResponse
	if err := p.client.GetJSON(ctx, allowedEndpoints, clusterStatusPath, p.cfg.TokenFile, &status); err != nil {
		result.State = extdiscovery.StateFor(err)
		result.Err = err
		return result
	}

	clusterID := clusterIDFrom(status)
	result.SourceID = clusterID

	// Step 2: the node list.
	var nodes nodeListResponse
	if err := p.client.GetJSON(ctx, allowedEndpoints, nodeListPath, p.cfg.TokenFile, &nodes); err != nil {
		result.State = extdiscovery.StateFor(err)
		result.Err = err
		return result
	}

	observed := make([]extdiscovery.Node, 0, len(nodes))
	complete := true

	for _, entry := range nodes {
		if entry.Node == "" {
			// A row with no node name cannot be identified, so it
			// cannot be recorded. It is skipped rather than
			// guessed at.
			complete = false
			continue
		}
		node := extdiscovery.Node{
			Provider:    ProviderName,
			SourceID:    clusterID,
			Name:        entry.Node,
			Fingerprint: entry.SSLFingerprint,
			Status:      entry.Status,
			ObservedAt:  now,
		}

		// Step 3: per-node detail. A failure here is PARTIAL, not
		// total: the node itself was seen, and only its detail is
		// missing.
		var detail nodeStatusResponse
		if err := p.client.GetJSON(ctx, allowedEndpoints, nodeStatusPath(entry.Node), p.cfg.TokenFile, &detail); err != nil {
			complete = false
		} else if detail.BootInfo.Mode != "" {
			// boot-info.mode ("efi" / "legacy-bios") is recorded as
			// part of the node's status suffix rather than as a
			// separate field, because Apiary's node record has no
			// field for it and inventing one here would give the
			// provider a schema it does not own.
			node.Status = joinStatus(node.Status, "boot="+detail.BootInfo.Mode)
		}

		observed = append(observed, node)
	}

	result.Nodes = observed

	if !complete {
		result.State = extdiscovery.StatePartial
		result.Complete = false
		return result
	}

	if len(observed) == 0 {
		result.State = extdiscovery.StateEmpty
		result.Complete = true
		return result
	}

	result.State = extdiscovery.StateFresh
	result.Complete = true
	return result
}

// clusterIDFrom extracts the cluster name from a /cluster/status
// response.
//
// A STANDALONE Proxmox node has no type:'cluster' row, and this
// returns "" for it. That empty string is the honest answer: the
// source did not name a cluster. It is NOT a sentinel that could
// collide with a real cluster name, because the composite identity
// carries the provider and the node name alongside it, and two
// standalone installations of the same provider are still distinct by
// their node names.
func clusterIDFrom(status clusterStatusResponse) string {
	for _, row := range status {
		if row.Type == "cluster" && row.Name != "" {
			return row.Name
		}
	}
	return ""
}

// joinStatus appends a suffix to a status string, tolerating an empty
// base.
func joinStatus(base, suffix string) string {
	if base == "" {
		return suffix
	}
	return base + " " + suffix
}
