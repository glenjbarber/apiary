package frontend

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/origincert"
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

// currentFrontendConfig/currentRestshimdConfig/currentRaftdConfig
// (ADR-0102) fetch the co-located sibling daemons' own settings,
// mirroring currentNodeConfig above exactly.
func (s *Server) currentFrontendConfig(r *http.Request) (frontendConfigView, string) {
	resp, err := s.client.GetFrontendConfig(r.Context(), &rpcpb.GetFrontendConfigRequest{})
	if err != nil {
		return frontendConfigView{}, err.Error()
	}
	if resp.GetError() != "" {
		return frontendConfigView{}, resp.GetError()
	}
	return fromRPCFrontendConfig(resp), ""
}

func (s *Server) currentRestshimdConfig(r *http.Request) (restshimdConfigView, string) {
	resp, err := s.client.GetRestshimdConfig(r.Context(), &rpcpb.GetRestshimdConfigRequest{})
	if err != nil {
		return restshimdConfigView{}, err.Error()
	}
	if resp.GetError() != "" {
		return restshimdConfigView{}, resp.GetError()
	}
	return fromRPCRestshimdConfig(resp), ""
}

func (s *Server) currentRaftdConfig(r *http.Request) (raftdConfigView, string) {
	resp, err := s.client.GetRaftdConfig(r.Context(), &rpcpb.GetRaftdConfigRequest{})
	if err != nil {
		return raftdConfigView{}, err.Error()
	}
	if resp.GetError() != "" {
		return raftdConfigView{}, resp.GetError()
	}
	return fromRPCRaftdConfig(resp), ""
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
	frontendCfg, frontendCfgErr := s.currentFrontendConfig(r)
	restshimdCfg, restshimdCfgErr := s.currentRestshimdConfig(r)
	raftdCfg, raftdCfgErr := s.currentRaftdConfig(r)
	vms, vmErr := s.currentMachineVMs(r, nodeID)
	cloudflareConfigured, _ := s.currentCloudflareStatus(r)
	services, serviceErr := s.currentNodeServices(r)
	uplinkStatus, uplinkErr := s.currentUplinkStatus(r)
	originCerts, originErr := s.currentOriginCertificates(r)

	s.render(w, "machine_page", s.withAuthFields(r, pageData{
		NodeConfig:               cfg,
		NodeConfigFormError:      cfgErr,
		FrontendConfig:           frontendCfg,
		FrontendConfigFormError:  frontendCfgErr,
		RestshimdConfig:          restshimdCfg,
		RestshimdConfigFormError: restshimdCfgErr,
		RaftdConfig:              raftdCfg,
		RaftdConfigFormError:     raftdCfgErr,
		MachineVMs:               vms,
		MachineFirewallError:     vmErr,
		CloudflareConfigured:     cloudflareConfigured,
		NodeServices:             services,
		ServiceFormError:         serviceErr,
		UplinkStatus:             uplinkStatus,
		UplinkFormError:          uplinkErr,
		OriginCertificates:       originCerts,
		OriginCAError:            originErr,
		JoinColonyResult:         s.currentJoinColonyResult(r),
		ActivePage:               "machine",
	}))
}

type originCertificateView struct {
	Name, Service, Hostnames, ExpiresAt string
	AutoRenew                           bool
	// Expiry is "ok", "soon", or "expired" - see origincert.ExpiryStatus,
	// computed here rather than trusting a stale value from the wire,
	// since "soon" and "expired" are both relative to the current time.
	Expiry string
}

func (s *Server) currentOriginCertificates(r *http.Request) ([]originCertificateView, string) {
	resp, err := s.client.ListOriginCertificates(r.Context(), &rpcpb.ListOriginCertificatesRequest{})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		return nil, resp.GetError()
	}
	now := time.Now()
	var out []originCertificateView
	for _, cert := range resp.GetCertificates() {
		expiresAt := time.Unix(cert.GetExpiresAtUnix(), 0)
		out = append(out, originCertificateView{
			Name: cert.GetName(), Service: cert.GetService(), Hostnames: strings.Join(cert.GetHostnames(), ", "),
			ExpiresAt: expiresAt.Local().Format("2006-01-02 15:04 MST"),
			AutoRenew: cert.GetAutoRenew(),
			Expiry:    string(origincert.InventoryEntry{ExpiresAt: expiresAt}.Expiry(now)),
		})
	}
	return out, ""
}

func (s *Server) handleIssueOriginCertificate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderOriginCAPanel(w, r, err.Error(), "")
		return
	}
	days, err := strconv.Atoi(r.FormValue("validity_days"))
	if err != nil {
		s.renderOriginCAPanel(w, r, "validity must be a whole number of days", "")
		return
	}
	var hostnames []string
	for _, host := range strings.Split(r.FormValue("hostnames"), ",") {
		if host = strings.TrimSpace(host); host != "" {
			hostnames = append(hostnames, host)
		}
	}
	resp, err := s.client.IssueOriginCertificate(r.Context(), &rpcpb.IssueOriginCertificateRequest{Name: strings.TrimSpace(r.FormValue("name")), Hostnames: hostnames, ValidityDays: int32(days), AutoRenew: r.Form.Has("auto_renew")})
	if err != nil {
		s.renderOriginCAPanel(w, r, err.Error(), "")
		return
	}
	if resp.GetError() != "" {
		s.renderOriginCAPanel(w, r, resp.GetError(), "")
		return
	}
	s.renderOriginCAPanel(w, r, "", "Certificate installed; apiary_managerd restart scheduled.")
}

func (s *Server) renderOriginCAPanel(w http.ResponseWriter, r *http.Request, formErr, success string) {
	certs, err := s.currentOriginCertificates(r)
	if err != "" && formErr == "" {
		formErr = err
	}
	cfg, cfgErr := s.currentNodeConfig(r)
	if cfgErr != "" && formErr == "" {
		formErr = cfgErr
	}
	s.render(w, "origin_ca_panel", pageData{NodeConfig: cfg,
		OriginCertificates: certs, OriginCAError: formErr, OriginCAOK: success,
		CanAdmin: true})
}

// currentNodeServices fetches the fixed set of Apiary rc.d services from
// this Hive's managerd. The result is intentionally local, like node config.
// apiary_managerd's own row is additionally annotated with the action-
// preflight restart guardrail's current verdict (ADR-0103) - a failed
// preflight call itself is treated as GuardrailBlocked too, matching the
// guardrail's own fail-closed posture, rather than silently showing a
// clean row.
func (s *Server) currentNodeServices(r *http.Request) ([]nodeServiceView, string) {
	resp, err := s.client.ListNodeServices(r.Context(), &rpcpb.ListNodeServicesRequest{})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		return nil, resp.GetError()
	}
	services := fromRPCNodeServices(resp)
	for i := range services {
		if services[i].Name != "apiary_managerd" {
			continue
		}
		preflight, perr := s.client.PreflightRestartNodeService(r.Context(), &rpcpb.PreflightRestartNodeServiceRequest{Name: services[i].Name})
		switch {
		case perr != nil:
			services[i].GuardrailBlocked = true
			services[i].GuardrailDetail = "could not check the restart guardrail: " + perr.Error()
		case preflight.GetError() != "":
			services[i].GuardrailBlocked = true
			services[i].GuardrailDetail = "could not check the restart guardrail: " + preflight.GetError()
		case preflight.GetVerdict() == "block" || preflight.GetVerdict() == "unknown":
			services[i].GuardrailBlocked = true
			if len(preflight.GetFindings()) > 0 {
				services[i].GuardrailDetail = preflight.GetFindings()[0].GetDetail()
			} else {
				services[i].GuardrailDetail = "restart is currently blocked"
			}
		}
	}
	return services, ""
}

// currentUplinkStatus fetches this Comb's own uplink interface state
// (ADR-0085) - intentionally local, like node config/services above.
func (s *Server) currentUplinkStatus(r *http.Request) (uplinkStatusView, string) {
	resp, err := s.client.GetUplinkStatus(r.Context(), &rpcpb.GetUplinkStatusRequest{})
	if err != nil {
		return uplinkStatusView{}, err.Error()
	}
	if resp.GetError() != "" {
		return uplinkStatusView{}, resp.GetError()
	}
	return fromRPCUplinkStatus(resp), ""
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
		PeerTlsCa:          cfg.PeerTLSCA,
		KnownPeerAddresses: cfg.KnownPeerAddresses,

		TlsCert:    cfg.TLSCert,
		TlsKey:     cfg.TLSKey,
		PamService: cfg.PAMService,

		CloudflareTokenFile:             cfg.CloudflareTokenFile,
		CloudflareZoneId:                cfg.CloudflareZoneID,
		CloudflareTunnelId:              cfg.CloudflareTunnelID,
		CloudflareTunnelCredentialsFile: cfg.CloudflareTunnelCredentialsFile,
		OriginCaTokenFile:               cfg.OriginCATokenFile,
		OriginCaDirectory:               cfg.OriginCADirectory,
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
	if r.Form.Has("peer_tls_ca") {
		req.PeerTlsCa = r.FormValue("peer_tls_ca")
	}
	if r.Form.Has("known_peer_addresses") {
		req.KnownPeerAddresses = r.FormValue("known_peer_addresses")
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
	if r.Form.Has("pam_service") {
		req.PamService = r.FormValue("pam_service")
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
	if r.Form.Has("origin_ca_token_file") {
		req.OriginCaTokenFile = r.FormValue("origin_ca_token_file")
	}
	if r.Form.Has("origin_ca_directory") {
		req.OriginCaDirectory = r.FormValue("origin_ca_directory")
	}
	return req
}

// handleUpdateFrontendConfig updates the co-located frontend's own
// settings (ADR-0102) - same combined-panel refresh pattern as
// handleUpdateMachineConfig, but against a separate RPC/response type
// since frontend has no config fields in common with managerd's own
// UpdateNodeConfig.
func (s *Server) handleUpdateFrontendConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderFrontendConfigPanel(w, r, "invalid form: "+err.Error())
		return
	}
	req := s.frontendConfigUpdateRequest(r)
	resp, err := s.client.UpdateFrontendConfig(r.Context(), req)
	if err != nil {
		s.renderFrontendConfigPanel(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderFrontendConfigPanel(w, r, resp.GetError())
		return
	}
	s.renderFrontendConfigPanel(w, r, "")
}

// frontendConfigUpdateRequest mirrors nodeConfigUpdateRequest's own
// resend-current-plus-r.Form.Has()-override pattern, for frontend's
// own settings. manager_api_key is write-only, same reasoning as
// peer_api_key on nodeConfigUpdateRequest - never read back from
// frontendConfigView (which never has the raw value to begin with),
// just forwarded as submitted.
func (s *Server) frontendConfigUpdateRequest(r *http.Request) *rpcpb.UpdateFrontendConfigRequest {
	cfg, _ := s.currentFrontendConfig(r)
	req := &rpcpb.UpdateFrontendConfigRequest{
		ManagerAddr:          cfg.ManagerAddr,
		HttpAddr:             cfg.HTTPAddr,
		ManagerTls:           cfg.ManagerTLS,
		ManagerTlsCa:         cfg.ManagerTLSCA,
		ManagerTlsServerName: cfg.ManagerTLSServerName,
		TlsCert:              cfg.TLSCert,
		TlsKey:               cfg.TLSKey,
		PeerTls:              cfg.PeerTLS,
		PeerTlsCa:            cfg.PeerTLSCA,
		PeerHostnameSuffix:   cfg.PeerHostnameSuffix,
		PeerManagerPort:      cfg.PeerManagerPort,
	}
	if r.Form.Has("manager_addr") {
		req.ManagerAddr = r.FormValue("manager_addr")
	}
	if r.Form.Has("http_addr") {
		req.HttpAddr = r.FormValue("http_addr")
	}
	if r.Form.Has("manager_tls") {
		req.ManagerTls = r.FormValue("manager_tls") == "true"
	}
	if r.Form.Has("manager_tls_ca") {
		req.ManagerTlsCa = r.FormValue("manager_tls_ca")
	}
	if r.Form.Has("manager_tls_server_name") {
		req.ManagerTlsServerName = r.FormValue("manager_tls_server_name")
	}
	if r.Form.Has("tls_cert") {
		req.TlsCert = r.FormValue("tls_cert")
	}
	if r.Form.Has("tls_key") {
		req.TlsKey = r.FormValue("tls_key")
	}
	if r.Form.Has("peer_tls") {
		req.PeerTls = r.FormValue("peer_tls") == "true"
	}
	if r.Form.Has("peer_tls_ca") {
		req.PeerTlsCa = r.FormValue("peer_tls_ca")
	}
	if r.Form.Has("peer_hostname_suffix") {
		req.PeerHostnameSuffix = r.FormValue("peer_hostname_suffix")
	}
	if r.Form.Has("peer_manager_port") {
		req.PeerManagerPort = r.FormValue("peer_manager_port")
	}
	if r.Form.Has("manager_api_key") {
		req.ManagerApiKey = r.FormValue("manager_api_key")
	}
	if r.FormValue("clear_manager_api_key") == "true" {
		req.ClearManagerApiKey = true
	}
	return req
}

func (s *Server) renderFrontendConfigPanel(w http.ResponseWriter, r *http.Request, formErr string) {
	cfg, fetchErr := s.currentFrontendConfig(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh: " + fetchErr
		}
	}
	s.render(w, "frontend_config_panel", pageData{FrontendConfig: cfg, FrontendConfigFormError: formErr, CanAdmin: true})
}

// handleUpdateRestshimdConfig mirrors handleUpdateFrontendConfig for
// the co-located restshimd (ADR-0102).
func (s *Server) handleUpdateRestshimdConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderRestshimdConfigPanel(w, r, "invalid form: "+err.Error())
		return
	}
	req := s.restshimdConfigUpdateRequest(r)
	resp, err := s.client.UpdateRestshimdConfig(r.Context(), req)
	if err != nil {
		s.renderRestshimdConfigPanel(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderRestshimdConfigPanel(w, r, resp.GetError())
		return
	}
	s.renderRestshimdConfigPanel(w, r, "")
}

// restshimdConfigUpdateRequest mirrors frontendConfigUpdateRequest,
// minus any secret handling - restshimdconfig.Config has none.
func (s *Server) restshimdConfigUpdateRequest(r *http.Request) *rpcpb.UpdateRestshimdConfigRequest {
	cfg, _ := s.currentRestshimdConfig(r)
	req := &rpcpb.UpdateRestshimdConfigRequest{
		ManagerAddr:          cfg.ManagerAddr,
		HttpAddr:             cfg.HTTPAddr,
		ManagerTls:           cfg.ManagerTLS,
		ManagerTlsCa:         cfg.ManagerTLSCA,
		ManagerTlsServerName: cfg.ManagerTLSServerName,
		TlsCert:              cfg.TLSCert,
		TlsKey:               cfg.TLSKey,
	}
	if r.Form.Has("manager_addr") {
		req.ManagerAddr = r.FormValue("manager_addr")
	}
	if r.Form.Has("http_addr") {
		req.HttpAddr = r.FormValue("http_addr")
	}
	if r.Form.Has("manager_tls") {
		req.ManagerTls = r.FormValue("manager_tls") == "true"
	}
	if r.Form.Has("manager_tls_ca") {
		req.ManagerTlsCa = r.FormValue("manager_tls_ca")
	}
	if r.Form.Has("manager_tls_server_name") {
		req.ManagerTlsServerName = r.FormValue("manager_tls_server_name")
	}
	if r.Form.Has("tls_cert") {
		req.TlsCert = r.FormValue("tls_cert")
	}
	if r.Form.Has("tls_key") {
		req.TlsKey = r.FormValue("tls_key")
	}
	return req
}

func (s *Server) renderRestshimdConfigPanel(w http.ResponseWriter, r *http.Request, formErr string) {
	cfg, fetchErr := s.currentRestshimdConfig(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh: " + fetchErr
		}
	}
	s.render(w, "restshimd_config_panel", pageData{RestshimdConfig: cfg, RestshimdConfigFormError: formErr, CanAdmin: true})
}

// raftd_config_panel (web/templates/machine.html) is read-only only -
// there is no handleUpdateRaftdConfig/raftdConfigUpdateRequest/
// renderRaftdConfigPanel at all. A 2026-09-15 audit found that a
// per-host save form is unsafe for raftd's internal_token/raft TLS
// material (see GetRaftdConfig's own doc comment in
// internal/manager/server.go) - the panel only ever calls
// currentRaftdConfig (a plain Get) from handleMachinePage.

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
// cert/key paths, and (ADR-0087) the PAM service name login is
// authenticated against - grouped here since the latter requires the
// former to be set.
func (s *Server) handleUpdateTLSConfig(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "tls_panel")
}

// handleUpdateCloudflareConfig updates ADR-0063's four Cloudflare
// Tunnel settings - lets an operator configure a Hive's Cloudflare
// Tunnel exposure through this page instead of only rc.conf.
func (s *Server) handleUpdateCloudflareConfig(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateMachineConfig(w, r, "cloudflare_config_panel")
}

// handleUpdateOriginCAConfig saves the two paths used only by the separate
// Origin CA lifecycle. Keeping this flow beside issuance avoids making the
// operator search the unrelated Tunnel configuration panel for a credential.
func (s *Server) handleUpdateOriginCAConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderOriginCAPanel(w, r, "invalid form: "+err.Error(), "")
		return
	}
	resp, err := s.client.UpdateNodeConfig(r.Context(), s.nodeConfigUpdateRequest(r))
	if err != nil {
		s.renderOriginCAPanel(w, r, err.Error(), "")
		return
	}
	if resp.GetError() != "" {
		s.renderOriginCAPanel(w, r, resp.GetError(), "")
		return
	}
	s.renderOriginCAPanel(w, r, "", "Origin CA configuration saved.")
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
	r.ParseForm()
	// force (ADR-0103) overrides the action-preflight restart guardrail
	// for apiary_managerd - an explicit "I understand this bypasses a
	// safety check" acknowledgment, only ever present when the panel
	// itself rendered the force checkbox because the guardrail was
	// already reporting Block/Unknown. Meaningless for every other
	// service, which the guardrail never gates.
	force := r.Form.Has("force")
	resp, err := s.client.RestartNodeService(r.Context(), &rpcpb.RestartNodeServiceRequest{Name: name, Force: force})
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
	if resp.GetGuardrailOverridden() {
		success += " (restart guardrail overridden by force)"
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

// handleSetUplinkState administratively brings this Comb's uplink
// interface down or back up (ADR-0085). This is a genuinely dangerous
// action if the caller's own network path shares the uplink interface -
// the confirmation is the form's own hx-confirm dialog (machine.html's
// uplink_panel), by explicit user choice; there is no automatic revert.
func (s *Server) handleSetUplinkState(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderUplinkPanel(w, r, "invalid form: "+err.Error(), "")
		return
	}
	down := r.FormValue("down") == "true"
	resp, err := s.client.SetUplinkState(r.Context(), &rpcpb.SetUplinkStateRequest{Down: down})
	if err != nil {
		s.renderUplinkPanel(w, r, err.Error(), "")
		return
	}
	if resp.GetError() != "" {
		s.renderUplinkPanel(w, r, resp.GetError(), "")
		return
	}
	success := "uplink brought down"
	if paused := resp.GetNatPausedNetworks(); len(paused) > 0 {
		success = fmt.Sprintf("uplink brought down (also paused outbound NAT for %d network(s): %s)", len(paused), strings.Join(paused, ", "))
	}
	if resp.GetUp() {
		success = "uplink brought back up"
	}
	s.renderUplinkPanel(w, r, "", success)
}

func (s *Server) renderUplinkPanel(w http.ResponseWriter, r *http.Request, formErr, success string) {
	status, fetchErr := s.currentUplinkStatus(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh: " + fetchErr
		}
	}
	s.render(w, "uplink_panel", pageData{
		UplinkStatus: status, UplinkFormError: formErr,
		UplinkFormSuccess: success, CanAdmin: true,
	})
}
