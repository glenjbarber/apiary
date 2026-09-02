package frontend

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// isoRowView is one row of the Images page's cluster-wide table
// (ADR-0040) - unlike the plain isoView (still used by the upload/
// delete result panel, a purely local operation), this tracks which
// known nodes actually have the file and which don't, driving the
// per-row "Copy to <node>"/"Copy to all missing" actions.
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
// failure shouldn't be indistinguishable from "click here to trigger a
// pointless self-copy".
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

// replicateISOTo dispatches a ReplicateISO call to targetNodeID: locally
// via s.client if it's this frontend's own colocated node, otherwise via
// s.peers - the same local-vs-peer dispatch nodeListISOs/nodeHostStats
// already use. sourceNodeID is any node already confirmed (by the
// caller) to have the file.
func (s *Server) replicateISOTo(ctx context.Context, targetNodeID, localNodeID, name, sourceNodeID string) (*rpcpb.ReplicateISOResponse, error) {
	req := &rpcpb.ReplicateISORequest{Name: name, SourceNodeId: sourceNodeID}
	if s.peers == nil || targetNodeID == localNodeID {
		return s.client.ReplicateISO(ctx, req)
	}
	return s.peers.ReplicateISO(ctx, s.peerAddr(targetNodeID), name, sourceNodeID)
}

// handleReplicateISO handles "Copy to <node>" (ADR-0040): copies name
// onto target_node_id from whichever node the row already confirmed has
// it. Synchronous, like the upload form itself - the response doesn't
// come back until the copy actually finishes or fails.
func (s *Server) handleReplicateISO(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	target := r.PathValue("target_node_id")

	rows, errMsg := s.currentClusterISOs(r)
	if errMsg != "" {
		s.renderClusterISOResult(w, r, fmt.Sprintf("refreshing cluster image list: %s", errMsg))
		return
	}
	var source string
	for _, row := range rows {
		if row.Name == name && len(row.PresentNodes) > 0 {
			source = row.PresentNodes[0]
			break
		}
	}
	if source == "" {
		s.renderClusterISOResult(w, r, fmt.Sprintf("%q is not present on any known node", name))
		return
	}

	localNodeID := s.localNodeIDOrEmpty(r)
	resp, err := s.replicateISOTo(r.Context(), target, localNodeID, name, source)
	if err != nil {
		s.renderClusterISOResult(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderClusterISOResult(w, r, resp.GetError())
		return
	}
	s.renderClusterISOResult(w, r, "")
}

// handleReplicateISOAll handles "Copy to all missing" (ADR-0040): loops
// handleReplicateISO's own per-target dispatch over every node this row
// currently lacks the file on, so a multi-node gap is one click. One
// target's failure doesn't stop the others - matching this project's
// established best-effort-per-item convention (internal/hoststats,
// internal/resetutil).
func (s *Server) handleReplicateISOAll(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	rows, errMsg := s.currentClusterISOs(r)
	if errMsg != "" {
		s.renderClusterISOResult(w, r, fmt.Sprintf("refreshing cluster image list: %s", errMsg))
		return
	}
	var source string
	var missing []string
	for _, row := range rows {
		if row.Name == name {
			if len(row.PresentNodes) > 0 {
				source = row.PresentNodes[0]
			}
			missing = row.MissingNodes
			break
		}
	}
	if source == "" {
		s.renderClusterISOResult(w, r, fmt.Sprintf("%q is not present on any known node", name))
		return
	}

	localNodeID := s.localNodeIDOrEmpty(r)
	var errs []string
	for _, target := range missing {
		resp, err := s.replicateISOTo(r.Context(), target, localNodeID, name, source)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", target, err.Error()))
			continue
		}
		if resp.GetError() != "" {
			errs = append(errs, fmt.Sprintf("%s: %s", target, resp.GetError()))
		}
	}
	if len(errs) > 0 {
		s.renderClusterISOResult(w, r, fmt.Sprintf("some copies failed: %v", errs))
		return
	}
	s.renderClusterISOResult(w, r, "")
}

// localNodeIDOrEmpty fetches this frontend's own colocated node ID, "" on
// failure - replicateISOTo's own nil-peers fallback still does the right
// thing (always dispatches locally) even if this comes back empty.
func (s *Server) localNodeIDOrEmpty(r *http.Request) string {
	resp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		return ""
	}
	return resp.GetManagerNodeId()
}

// renderClusterISOResult re-renders the Images page's cluster table
// alongside a result message - mirroring renderISOPanelResult's own
// combined-target pattern for the (separate, local-only) upload/delete
// panel.
func (s *Server) renderClusterISOResult(w http.ResponseWriter, r *http.Request, formErr string) {
	rows, fetchErr := s.currentClusterISOs(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh list: " + fetchErr
		}
	}
	s.render(w, "cluster_iso_panel", pageData{ClusterISOs: rows, ClusterISOFormError: formErr})
}
