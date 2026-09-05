package frontend

import (
	"net/http"
	"strconv"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// currentNodeConfig fetches this node's own local settings (ADR-0049).
func (s *Server) currentNodeConfig(r *http.Request) (nodeConfigView, string) {
	resp, err := s.client.GetNodeConfig(r.Context(), &rpcpb.GetNodeConfigRequest{})
	if err != nil {
		return nodeConfigView{}, err.Error()
	}
	if resp.GetError() != "" {
		return nodeConfigView{}, resp.GetError()
	}
	return fromRPCNodeConfig(resp), ""
}

// currentMachineVMs fetches every VM (reusing currentVMs, which already
// forwards to the leader when needed - ADR-0035) and filters down to
// the ones assigned to this node, for the firewall-pause table. There
// is no local-only VM listing RPC on the external API (ListVMsLocal is
// internal-only, used by the reconciler itself), so filtering
// client-side against the already-fetched full list is simplest.
func (s *Server) currentMachineVMs(r *http.Request, localNodeID string) ([]vmView, string) {
	vms, errMsg := s.currentVMs(r, "id", "asc")
	if errMsg != "" {
		return nil, errMsg
	}
	local := make([]vmView, 0, len(vms))
	for _, vm := range vms {
		if vm.NodeID == localNodeID {
			local = append(local, vm)
		}
	}
	return local, ""
}

// localNodeID fetches this managerd's own node id, the same way
// handleNewVMPage does - best-effort, empty on failure rather than an
// error, since a page render with an empty machine-VMs table is still
// useful.
func (s *Server) localNodeID(r *http.Request) string {
	resp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		return ""
	}
	return resp.GetManagerNodeId()
}

// handleMachinePage serves the Machine Configuration page ("/machine",
// ADR-0049): this node's uplink settings, a firewall-pause table for
// VMs assigned to this node, and a dataset-quota form.
func (s *Server) handleMachinePage(w http.ResponseWriter, r *http.Request) {
	nodeID := s.localNodeID(r)
	cfg, cfgErr := s.currentNodeConfig(r)
	vms, vmErr := s.currentMachineVMs(r, nodeID)
	cloudflareConfigured, _ := s.currentCloudflareStatus(r)

	s.render(w, "machine_page", s.withAuthFields(r, pageData{
		NodeConfig:           cfg,
		NodeConfigFormError:  cfgErr,
		MachineVMs:           vms,
		MachineFirewallError: vmErr,
		CloudflareConfigured: cloudflareConfigured,
		ActivePage:           "machine",
	}))
}

// currentCloudflareStatus reports whether this node's own managerd has
// Cloudflare Tunnel exposure configured (ADR-0063) - best-effort, false
// on a fetch failure rather than an error, since the Machine page's
// setup-instructions panel is equally correct either way (a fetch
// failure just means "assume unconfigured, show the setup steps").
func (s *Server) currentCloudflareStatus(r *http.Request) (bool, string) {
	resp, err := s.client.HostStats(r.Context(), &rpcpb.HostStatsRequest{})
	if err != nil {
		return false, err.Error()
	}
	return resp.GetCloudflareConfigured(), ""
}

// handleUpdateNodeConfig follows the same combined-panel pattern as
// handleCreateNetwork: refresh just the #nodeconfig-panel (error slot +
// current values together). Takes effect on this node's next managerd
// restart, not live - see ADR-0049.
func (s *Server) handleUpdateNodeConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderNodeConfigPanel(w, r, "invalid form: "+err.Error())
		return
	}
	resp, err := s.client.UpdateNodeConfig(r.Context(), &rpcpb.UpdateNodeConfigRequest{
		Uplink:    r.FormValue("uplink"),
		NatUplink: r.FormValue("nat_uplink"),
	})
	if err != nil {
		s.renderNodeConfigPanel(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderNodeConfigPanel(w, r, resp.GetError())
		return
	}
	s.renderNodeConfigPanel(w, r, "")
}

func (s *Server) renderNodeConfigPanel(w http.ResponseWriter, r *http.Request, formErr string) {
	cfg, fetchErr := s.currentNodeConfig(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh: " + fetchErr
		}
	}
	s.render(w, "nodeconfig_panel", pageData{NodeConfig: cfg, NodeConfigFormError: formErr})
}

// handleSetVMFirewallPaused toggles one VM's firewall_paused (ADR-0049) -
// paused is a plain "true"/"false" form value rather than a checkbox,
// since each row renders a single always-appropriate Pause/Resume
// button (the same one-button-per-row convention the VM table's own
// Delete button already follows), not an editable form field.
func (s *Server) handleSetVMFirewallPaused(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.renderMachineVMsPanel(w, r, "invalid form: "+err.Error())
		return
	}
	paused, err := strconv.ParseBool(r.FormValue("paused"))
	if err != nil {
		s.renderMachineVMsPanel(w, r, "invalid paused value: "+err.Error())
		return
	}
	resp, err := s.client.SetVMFirewallPaused(r.Context(), &rpcpb.SetVMFirewallPausedRequest{Id: id, Paused: paused})
	if err != nil {
		s.renderMachineVMsPanel(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderMachineVMsPanel(w, r, resp.GetError())
		return
	}
	s.renderMachineVMsPanel(w, r, "")
}

func (s *Server) renderMachineVMsPanel(w http.ResponseWriter, r *http.Request, formErr string) {
	nodeID := s.localNodeID(r)
	vms, fetchErr := s.currentMachineVMs(r, nodeID)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh list: " + fetchErr
		}
	}
	s.render(w, "machine_vms_panel", pageData{MachineVMs: vms, MachineFirewallError: formErr})
}

// handleSetDatasetQuota sets a ZFS quota on a dataset under this node's
// own configured Base scope (ADR-0049) - a plain fire-and-report form,
// no live dataset/quota listing (a "starter", not a full storage
// management UI).
func (s *Server) handleSetDatasetQuota(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderQuotaPanel(w, r, "invalid form: "+err.Error(), "")
		return
	}
	name := r.FormValue("dataset_name")
	quota := r.FormValue("quota")
	resp, err := s.client.SetDatasetQuota(r.Context(), &rpcpb.SetDatasetQuotaRequest{DatasetName: name, Quota: quota})
	if err != nil {
		s.renderQuotaPanel(w, r, err.Error(), "")
		return
	}
	if resp.GetError() != "" {
		s.renderQuotaPanel(w, r, resp.GetError(), "")
		return
	}
	s.renderQuotaPanel(w, r, "", "set "+quota+" on "+name)
}

func (s *Server) renderQuotaPanel(w http.ResponseWriter, r *http.Request, formErr, success string) {
	s.render(w, "quota_panel", pageData{QuotaFormError: formErr, QuotaFormSuccess: success})
}
