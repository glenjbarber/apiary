package frontend

// The replica-node picker, as a fragment the Placement pane refreshes.
//
// WHY A ROUND-TRIP. The rule is "the replica cannot be the owner", and
// the owner is an answer the operator gives in the same pane. So the
// list cannot be right until the owner is known.
//
// Two ways to do that. The browser could hide the option with
// JavaScript - which is what the template does today - or the server
// could render the list with the owner's current answer applied.
//
// The second is the one that holds. The spec's own account of the
// placement gap is that "client JS mirroring server rules is exactly
// how the placement-pane gap happened": the rule lived in Go, the
// template's JS never learned about it, and the two drifted.
//
// So the rule lives in Go (`replicaNodeOptions`), the fragment renders
// it, and the template asks. The JavaScript that MIMICS it is deleted,
// because a second copy of a rule is a second chance to be wrong.

import (
	"net/http"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// handleReplicaNodeOptions renders the replica-node picker with the
// owner excluded.
//
// It is a GET and it is READ-ONLY: it changes nothing, and it is
// reachable by the same role as the wizard itself.
func (s *Server) handleReplicaNodeOptions(w http.ResponseWriter, r *http.Request) {
	owner := strings.TrimSpace(r.URL.Query().Get("owner"))

	nodes, err := s.knownNodes(r)
	if err != nil {
		// Fail-soft, like every other list in this app: a Comb that
		// cannot be listed leaves the picker showing what it has.
		nodes = nil
	}
	s.renderReplicaNodeOptions(w, nodes, owner)
}

// renderReplicaNodeOptions renders the picker itself, so the full page
// and the fragment share one rendering and cannot disagree.
func (s *Server) renderReplicaNodeOptions(w http.ResponseWriter, nodes []string, owner string) {
	options := replicaNodeOptions(nodes, owner)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(options) == 0 {
		// Nothing to offer. The field is disabled rather than
		// hidden, so the operator can see that a replica is a thing
		// this Comb cannot have rather than wondering where it went.
		_, _ = w.Write([]byte(`<select name="replica_node_id" id="replica-node" disabled title="No Comb other than the owner can hold a replica.">` +
			`<option value="">None</option></select>`))
		return
	}
	var b strings.Builder
	b.WriteString(`<select name="replica_node_id" id="replica-node" title="A second Comb to keep a live HAST-replicated copy of this workload's disk on. Leave as None for no replica.">`)
	b.WriteString(`<option value="">None</option>`)
	for _, node := range options {
		b.WriteString(`<option value="` + node + `">` + node + `</option>`)
	}
	b.WriteString(`</select>`)
	_, _ = w.Write([]byte(b.String()))
}

// the RPC is referenced so the response shape stays checked.
var _ = rpcpb.GetNodeConfigRequest{}
