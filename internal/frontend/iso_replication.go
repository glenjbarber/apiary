package frontend

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"sync"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// isoRowView is the cluster-wide view of one stored image (ADR-0041,
// superseding ADR-0040's manual per-node copy UI) - which known nodes
// already have it (PresentNodes) and which don't (MissingNodes). Used
// by the create-VM/create-jail forms' image pickers to show a "will be
// fetched from a peer" cue for whichever node the operator selects,
// since internal/cluster's Reconciler now fetches a missing image
// automatically at provisioning time rather than requiring a manual
// copy first.
type isoRowView struct {
	Name         string
	SizeBytes    uint64
	SHA256       string
	PresentNodes []string
	MissingNodes []string
}

// currentClusterISOs fetches every known node's own ListISOs
// concurrently (mirroring handleClusterOverviewPage's own concurrent
// per-node fetch pattern exactly - one slow/unreachable node shouldn't
// hold up the rest) and merges the results by (name, sha256) into one
// row per distinct file. A node that fails to answer at all is simply
// treated as not having anything - the same fail-soft posture
// currentVMs/currentISOs already follow, since a transient fetch
// failure shouldn't block the create-VM form from rendering at all.
func (s *Server) currentClusterISOs(r *http.Request) ([]isoRowView, string) {
	statusResp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		return nil, err.Error()
	}
	localNodeID := statusResp.GetManagerNodeId()
	nodeIDs := statusResp.GetKnownNodeIds()
	if len(nodeIDs) == 0 && localNodeID != "" {
		nodeIDs = []string{localNodeID}
	}

	type nodeResult struct {
		nodeID string
		isos   []*rpcpb.ISOInfo
	}
	results := make([]nodeResult, len(nodeIDs))
	var wg sync.WaitGroup
	for i, id := range nodeIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			resp, err := s.nodeListISOs(r.Context(), id, localNodeID)
			if err != nil {
				return
			}
			results[i] = nodeResult{nodeID: id, isos: resp.GetIsos()}
		}(i, id)
	}
	wg.Wait()

	type key struct{ name, sha256 string }
	rows := map[key]*isoRowView{}
	for _, res := range results {
		for _, info := range res.isos {
			k := key{info.GetName(), info.GetSha256()}
			row, ok := rows[k]
			if !ok {
				row = &isoRowView{Name: info.GetName(), SizeBytes: info.GetSizeBytes(), SHA256: info.GetSha256()}
				rows[k] = row
			}
			row.PresentNodes = append(row.PresentNodes, res.nodeID)
		}
	}

	out := make([]isoRowView, 0, len(rows))
	for _, row := range rows {
		present := map[string]bool{}
		for _, id := range row.PresentNodes {
			present[id] = true
		}
		for _, id := range nodeIDs {
			if !present[id] {
				row.MissingNodes = append(row.MissingNodes, id)
			}
		}
		sort.Strings(row.PresentNodes)
		sort.Strings(row.MissingNodes)
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, ""
}

// isoMissingByNode renders rows as a JS object literal mapping each
// image's name to its MissingNodes list, e.g.
// {"ubuntu.iso":["freebsd-apiary2"],"base.img":[]} - embedded directly
// into the create-VM/create-jail forms so their own vanilla JS (no
// server round-trip needed) can show a "will be fetched from a peer"
// cue next to whichever image is missing from the currently-selected
// Node ID, updating live as that selection changes.
func isoMissingByNode(rows []isoRowView) (template.JS, error) {
	m := make(map[string][]string, len(rows))
	for _, row := range rows {
		missing := row.MissingNodes
		if missing == nil {
			missing = []string{}
		}
		m[row.Name] = missing
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return template.JS(b), nil
}

// nodeListISOs fetches nodeID's ListISOs: directly through s.client if
// nodeID is this frontend's own colocated node (or peers isn't
// configured at all), otherwise by dialing that node's managerd
// directly via s.peers - mirroring nodeHostStats's own local-vs-peer
// dispatch exactly.
func (s *Server) nodeListISOs(ctx context.Context, nodeID, localNodeID string) (*rpcpb.ListISOsResponse, error) {
	if s.peers == nil || nodeID == localNodeID {
		return s.client.ListISOs(ctx, &rpcpb.ListISOsRequest{})
	}
	return s.peers.ListISOs(ctx, s.peerAddr(nodeID))
}
