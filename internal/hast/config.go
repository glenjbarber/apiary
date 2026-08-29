package hast

import (
	"fmt"
	"strings"
)

// DefaultConfigPath is where hastd(8)/hastctl(8) read configuration from
// by default.
const DefaultConfigPath = "/etc/hast.conf"

// Node is one resource's per-node section: "on Name { local Local;
// remote Remote }" in hast.conf's terms.
type Node struct {
	// Name identifies the node as hastd expects: a hostname (full or
	// short) matching gethostname(3) on that node.
	Name string

	// Local is the local GEOM provider path on that node (e.g.
	// /dev/da1, or a file-backed md(4) device for testing).
	Local string

	// Remote is the address (host or host:port) of the *other* node,
	// used by this node to connect to (if primary) or accept
	// connections from (if secondary) its peer.
	Remote string
}

// Resource is one HAST resource: a replicated provider shared between
// exactly two nodes. The same rendered config is deployed identically to
// both nodes; each node's hastd determines which Node section describes
// itself by matching its own hostname.
type Resource struct {
	Name  string
	Nodes []Node
}

// RenderConfig renders a full /etc/hast.conf body for the given
// resources. This is pure and requires no HAST tooling to test.
func RenderConfig(resources []Resource) (string, error) {
	var b strings.Builder
	for _, r := range resources {
		if r.Name == "" {
			return "", fmt.Errorf("hast: resource name must not be empty")
		}
		if len(r.Nodes) != 2 {
			return "", fmt.Errorf("hast: resource %q must have exactly 2 nodes, got %d", r.Name, len(r.Nodes))
		}

		fmt.Fprintf(&b, "resource %s {\n", r.Name)
		for _, n := range r.Nodes {
			if n.Name == "" || n.Local == "" || n.Remote == "" {
				return "", fmt.Errorf("hast: resource %q: node Name, Local, and Remote must all be set", r.Name)
			}
			fmt.Fprintf(&b, "  on %s {\n", n.Name)
			fmt.Fprintf(&b, "    local %s\n", n.Local)
			fmt.Fprintf(&b, "    remote %s\n", n.Remote)
			b.WriteString("  }\n")
		}
		b.WriteString("}\n")
	}
	return b.String(), nil
}
