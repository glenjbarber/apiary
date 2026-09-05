package frontend

import (
	"net/http"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// placementHiveView distinguishes known facts from an unavailable probe. It
// informs a human's explicit placement choice; it is not a scheduler.
type placementHiveView struct {
	NodeID      string
	VMCapable   bool
	JailCapable bool
	JailKnown   bool
	ProbeError  string
}

func (s *Server) currentPlacementHives(r *http.Request, nodes []string, localNodeID string) []placementHiveView {
	views := make([]placementHiveView, 0, len(nodes))
	for _, nodeID := range nodes {
		view := placementHiveView{NodeID: nodeID}
		var hs *rpcpb.HostStatsResponse
		var cfg *rpcpb.GetNodeConfigResponse
		var err error
		if s.peers == nil || nodeID == localNodeID {
			hs, err = s.client.HostStats(r.Context(), &rpcpb.HostStatsRequest{})
			if err == nil {
				cfg, err = s.client.GetNodeConfig(r.Context(), &rpcpb.GetNodeConfigRequest{})
			}
		} else {
			hs, err = s.peers.HostStats(r.Context(), s.peerAddr(nodeID))
			if err == nil {
				cfg, err = s.peers.GetNodeConfig(r.Context(), s.peerAddr(nodeID))
			}
		}
		if err != nil {
			view.ProbeError = err.Error()
			views = append(views, view)
			continue
		}
		view.VMCapable = hs.GetBhyveConfigured()
		if cfg.JailEnabled != nil {
			view.JailKnown = true
			view.JailCapable = cfg.GetJailEnabled()
		}
		views = append(views, view)
	}
	return views
}
