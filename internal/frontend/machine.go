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
	services, serviceErr := s.currentNodeServices(r)

	s.render(w, "machine_page", s.withAuthFields(r, pageData{
		NodeConfig:           cfg,
		NodeConfigFormError:  cfgErr,
		MachineVMs:           vms,
		MachineFirewallError: vmErr,
		CloudflareConfigured: cloudflareConfigured,
		NodeServices:         services,
		ServiceFormError:     serviceErr,
		ActivePage:           "machine",
	}))
}

// currentNodeServices fetches the fixed set of Apiary rc.d services from
// this Hive's managerd. The result is intentionally local, like node config.
func (s *Server) currentNodeServices(r *http.Request) ([]nodeServiceView, string) {
	resp, err := s.client.ListNodeServices(r.Context(), &rpcpb.ListNodeServicesRequest{})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		return nil, resp.GetError()
	}
	return fromRPCNodeServices(resp), ""
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
	s.handleUpdateMachineConfig(w, r, "nodeconfig_panel")
}

// handleUpdateJailProvisioning updates the same underlying node config
// as handleUpdateNodeConfig, but refreshes the dedicated jail panel so
// the setting can live in its own visible section on the Machine page.
func (s *Server) handleUpdateJailProvisioning(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "jail_provisioning_panel")
}

func (s *Server) handleUpdateMachineConfig(w http.ResponseWriter, r *http.Request, panel string) {
	if err := r.ParseForm(); err != nil {
		s.renderMachineConfigPanel(w, r, panel, "invalid form: "+err.Error())
		return
	}
	req := s.nodeConfigUpdateRequest(r)
	resp, err := s.client.UpdateNodeConfig(r.Context(), req)
	if err != nil {
		s.renderMachineConfigPanel(w, r, panel, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderMachineConfigPanel(w, r, panel, resp.GetError())
		return
	}
	s.renderMachineConfigPanel(w, r, panel, "")
}

// nodeConfigUpdateRequest builds a full UpdateNodeConfig request,
// starting from every currently-saved value (Save replaces the whole
// file, not a merge - see nodeconfig.Manager.Save's own doc comment)
// and overriding only the fields the submitting form actually carries
// (checked via r.Form.Has, the same convention every field here
// already followed before ADR-0070's expansion). This is shared by
// every Machine Configuration panel's own POST handler - each panel's
// form only includes the fields it displays, plus hidden inputs for
// anything else it needs to avoid accidentally clearing (see
// machine.html's own per-panel hidden-input pattern).
//
// peer_api_key/raftd_token are handled differently, matching
// UpdateNodeConfigRequest's own write-only design: this function never
// reads a "current" secret value back out of nodeConfigView (which
// never has one to begin with - GetNodeConfig never returns it), it
// just forwards whatever the form submitted (new value, clear flag, or
// neither) straight through, and UpdateNodeConfig's own RPC handler is
// what actually implements "empty means leave unchanged."
func (s *Server) nodeConfigUpdateRequest(r *http.Request) *rpcpb.UpdateNodeConfigRequest {
	cfg, _ := s.currentNodeConfig(r)
	req := &rpcpb.UpdateNodeConfigRequest{
		Uplink:        cfg.Uplink,
		NatUplink:     cfg.NATUplink,
		DhcpDnsServer: cfg.DNSServer,
		JailEnabled:   jailEnabledFromForm(cfg.JailEnabledMode),

		ZfsBase:       cfg.ZFSBase,
		BhyvePrefix:   cfg.BhyvePrefix,
		IsoDir:        cfg.ISODir,
		JailPrefix:    cfg.JailPrefix,
		JailMountBase: cfg.JailMountBase,

		ReconcileInterval:           cfg.ReconcileInterval,
		AssumptionCheckInterval:     cfg.AssumptionCheckInterval,
		AssumptionHeartbeatInterval: cfg.AssumptionHeartbeatInterval,
		AssumptionStaleAfter:        cfg.AssumptionStaleAfter,
		AssumptionRunDeadline:       cfg.AssumptionRunDeadline,
		AssumptionHistoryMaxAge:     cfg.AssumptionHistoryMaxAge,
		AssumptionHistoryLimit:      cfg.AssumptionHistoryLimit,

		BhyveBootrom:   cfg.BhyveBootROM,
		BhyveBridge:    cfg.BhyveBridge,
		DiskSizeMb:     cfg.DiskSizeMB,
		JailDiskSizeMb: cfg.JailDiskSizeMB,

		HastEnabled: jailEnabledFromForm(cfg.HASTEnabledMode),
		PeerTls:     jailEnabledFromForm(cfg.PeerTLSMode),

		PeerManagerdPort:   cfg.PeerManagerdPort,
		PeerTlsHostnameMap: cfg.PeerTLSHostnameMap,

		TlsCert: cfg.TLSCert,
		TlsKey:  cfg.TLSKey,

		CloudflareTokenFile:             cfg.CloudflareTokenFile,
		CloudflareZoneId:                cfg.CloudflareZoneID,
		CloudflareTunnelId:              cfg.CloudflareTunnelID,
		CloudflareTunnelCredentialsFile: cfg.CloudflareTunnelCredentialsFile,
	}
	if r.Form.Has("uplink") {
		req.Uplink = r.FormValue("uplink")
	}
	if r.Form.Has("nat_uplink") {
		req.NatUplink = r.FormValue("nat_uplink")
	}
	if r.Form.Has("dhcp_dns_server") {
		req.DhcpDnsServer = r.FormValue("dhcp_dns_server")
	}
	if r.Form.Has("jail_enabled") {
		req.JailEnabled = jailEnabledFromForm(r.FormValue("jail_enabled"))
	}
	if r.Form.Has("zfs_base") {
		req.ZfsBase = r.FormValue("zfs_base")
	}
	if r.Form.Has("bhyve_prefix") {
		req.BhyvePrefix = r.FormValue("bhyve_prefix")
	}
	if r.Form.Has("iso_dir") {
		req.IsoDir = r.FormValue("iso_dir")
	}
	if r.Form.Has("jail_prefix") {
		req.JailPrefix = r.FormValue("jail_prefix")
	}
	if r.Form.Has("jail_mount_base") {
		req.JailMountBase = r.FormValue("jail_mount_base")
	}
	if r.Form.Has("reconcile_interval") {
		req.ReconcileInterval = r.FormValue("reconcile_interval")
	}
	if r.Form.Has("assumption_check_interval") {
		req.AssumptionCheckInterval = r.FormValue("assumption_check_interval")
	}
	if r.Form.Has("assumption_heartbeat_interval") {
		req.AssumptionHeartbeatInterval = r.FormValue("assumption_heartbeat_interval")
	}
	if r.Form.Has("assumption_stale_after") {
		req.AssumptionStaleAfter = r.FormValue("assumption_stale_after")
	}
	if r.Form.Has("assumption_run_deadline") {
		req.AssumptionRunDeadline = r.FormValue("assumption_run_deadline")
	}
	if r.Form.Has("assumption_history_max_age") {
		req.AssumptionHistoryMaxAge = r.FormValue("assumption_history_max_age")
	}
	if r.Form.Has("assumption_history_limit") {
		if n, err := strconv.ParseInt(r.FormValue("assumption_history_limit"), 10, 32); err == nil {
			req.AssumptionHistoryLimit = int32(n)
		}
	}
	if r.Form.Has("bhyve_bootrom") {
		req.BhyveBootrom = r.FormValue("bhyve_bootrom")
	}
	if r.Form.Has("bhyve_bridge") {
		req.BhyveBridge = r.FormValue("bhyve_bridge")
	}
	if r.Form.Has("disk_size_mb") {
		if n, err := strconv.ParseUint(r.FormValue("disk_size_mb"), 10, 64); err == nil {
			req.DiskSizeMb = n
		}
	}
	if r.Form.Has("jail_disk_size_mb") {
		if n, err := strconv.ParseUint(r.FormValue("jail_disk_size_mb"), 10, 64); err == nil {
			req.JailDiskSizeMb = n
		}
	}
	if r.Form.Has("hast_enabled") {
		req.HastEnabled = jailEnabledFromForm(r.FormValue("hast_enabled"))
	}
	if r.Form.Has("peer_tls") {
		req.PeerTls = jailEnabledFromForm(r.FormValue("peer_tls"))
	}
	if r.Form.Has("peer_managerd_port") {
		req.PeerManagerdPort = r.FormValue("peer_managerd_port")
	}
	if r.Form.Has("peer_tls_hostname_map") {
		req.PeerTlsHostnameMap = r.FormValue("peer_tls_hostname_map")
	}
	if r.Form.Has("peer_api_key") {
		req.PeerApiKey = r.FormValue("peer_api_key")
	}
	if r.FormValue("clear_peer_api_key") == "true" {
		req.ClearPeerApiKey = true
	}
	if r.Form.Has("raftd_token") {
		req.RaftdToken = r.FormValue("raftd_token")
	}
	if r.FormValue("clear_raftd_token") == "true" {
		req.ClearRaftdToken = true
	}
	if r.Form.Has("tls_cert") {
		req.TlsCert = r.FormValue("tls_cert")
	}
	if r.Form.Has("tls_key") {
		req.TlsKey = r.FormValue("tls_key")
	}
	if r.Form.Has("cloudflare_token_file") {
		req.CloudflareTokenFile = r.FormValue("cloudflare_token_file")
	}
	if r.Form.Has("cloudflare_zone_id") {
		req.CloudflareZoneId = r.FormValue("cloudflare_zone_id")
	}
	if r.Form.Has("cloudflare_tunnel_id") {
		req.CloudflareTunnelId = r.FormValue("cloudflare_tunnel_id")
	}
	if r.Form.Has("cloudflare_tunnel_credentials_file") {
		req.CloudflareTunnelCredentialsFile = r.FormValue("cloudflare_tunnel_credentials_file")
	}
	return req
}

// handleUpdateResourceScope updates the five write-once resource-scope
// paths (ADR-0070) - a dedicated panel/handler since these need the
// "read-only once set" template behavior none of the other panels do.
func (s *Server) handleUpdateResourceScope(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "resource_scope_panel")
}

// handleUpdateBhyveConfig updates bhyve/VM provisioning tuning
// (BhyveBootROM, BhyveBridge, DiskSizeMB, ReconcileInterval) - all
// freely editable any time, unlike the resource-scope paths above.
func (s *Server) handleUpdateBhyveConfig(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "bhyve_config_panel")
}

// handleUpdateHASTProvisioning mirrors handleUpdateJailProvisioning for
// -hast-enabled.
func (s *Server) handleUpdateHASTProvisioning(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "hast_panel")
}

// handleUpdatePeerForwarding updates peer-to-peer reconciler-forwarding
// settings (ADR-0029), including the write-only peer_api_key secret.
func (s *Server) handleUpdatePeerForwarding(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "peer_forwarding_panel")
}

// handleUpdateTLSConfig updates this node's own external gRPC TLS
// cert/key paths.
func (s *Server) handleUpdateTLSConfig(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "tls_panel")
}

// handleUpdateCloudflareConfig updates ADR-0063's four Cloudflare
// Tunnel settings - lets an operator configure a Hive's Cloudflare
// Tunnel exposure through this page instead of only rc.conf.
func (s *Server) handleUpdateCloudflareConfig(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "cloudflare_config_panel")
}

// handleUpdateAssumptionTuning updates the six Automated Assumption
// Checks tuning knobs (ADR-0055) - pure timing/retention settings, safe
// to change any time.
func (s *Server) handleUpdateAssumptionTuning(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "assumption_tuning_panel")
}

// handleUpdateInternalSecurity updates the write-only raftd_token
// secret (ADR-0033).
func (s *Server) handleUpdateInternalSecurity(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "internal_security_panel")
}

func jailEnabledFromForm(v string) *bool {
	switch v {
	case "enabled":
		enabled := true
		return &enabled
	case "disabled":
		enabled := false
		return &enabled
	default:
		return nil
	}
}

func (s *Server) renderNodeConfigPanel(w http.ResponseWriter, r *http.Request, formErr string) {
	s.renderMachineConfigPanel(w, r, "nodeconfig_panel", formErr)
}

func (s *Server) renderMachineConfigPanel(w http.ResponseWriter, r *http.Request, panel, formErr string) {
	cfg, fetchErr := s.currentNodeConfig(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh: " + fetchErr
		}
	}
	s.render(w, panel, pageData{NodeConfig: cfg, NodeConfigFormError: formErr, CanAdmin: true})
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

// handleRestartNodeService restarts a managerd-approved Apiary rc.d service
// on this Hive. managerd self-restarts are accepted asynchronously so the
// current gRPC request can finish before its process is replaced.
func (s *Server) handleRestartNodeService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	resp, err := s.client.RestartNodeService(r.Context(), &rpcpb.RestartNodeServiceRequest{Name: name})
	if err != nil {
		s.renderNodeServicesPanel(w, r, err.Error(), "")
		return
	}
	if resp.GetError() != "" {
		s.renderNodeServicesPanel(w, r, resp.GetError(), "")
		return
	}
	success := name + " restarted"
	if resp.GetScheduled() {
		success = name + " restart scheduled"
	}
	s.renderNodeServicesPanel(w, r, "", success)
}

func (s *Server) renderNodeServicesPanel(w http.ResponseWriter, r *http.Request, formErr, success string) {
	services, fetchErr := s.currentNodeServices(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh: " + fetchErr
		}
	}
	s.render(w, "node_services_panel", pageData{
		NodeServices: services, ServiceFormError: formErr,
		ServiceFormSuccess: success, CanAdmin: true,
	})
}
