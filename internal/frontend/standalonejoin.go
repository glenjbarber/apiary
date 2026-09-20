// The standalone-Comb-to-Colony-joiner conversion action (ADR-0105): a
// narrow, Admin-only Machine page action distinct from the "Join a
// Colony" panel in joincolony.go - that panel is for a genuinely fresh
// node with no state of its own; this one is for converting an
// already-bootstrapped standalone Comb, which needs its own explicit
// acknowledgement and irreversible-work guard.
package frontend

import (
	"net/http"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// convertToJoinerConfirmPhrase mirrors internal/manager's own
// convertToJoinerConfirmPhrase exactly (duplicated, not imported, the
// same reasoning as raftdservice.go's own raftdResetConfirmPhrase
// duplication of cmd/raftd's constant) - used only to pre-fill the
// form's own placeholder text, never to bypass the server's real check.
const convertToJoinerConfirmPhrase = "yes-convert-to-joiner"

// handleConvertStandaloneToJoiner implements POST /machine/convert-to-joiner.
// Admin-only (enforced both here via CanAdmin and server-side by the RPC's
// own auth tier) - the confirm_phrase form field must be typed exactly,
// never a checkbox, matching the spec's "an exact, visible acknowledgement"
// requirement.
func (s *Server) handleConvertStandaloneToJoiner(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderMachinePageWithConvertJoinerError(w, r, err.Error())
		return
	}
	resp, err := s.client.ConvertStandaloneToJoiner(r.Context(), &rpcpb.ConvertStandaloneToJoinerRequest{
		TargetManagerdAddress: withFixedPort(r.FormValue("target_managerd_host"), managerdListenerPort),
		RaftBind:              withFixedPort(r.FormValue("raft_bind_host"), raftdListenerPort),
		ConfirmPhrase:         r.FormValue("confirm_phrase"),
	})
	if err != nil {
		s.renderMachinePageWithConvertJoinerError(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderMachinePageWithConvertJoinerError(w, r, resp.GetError())
		return
	}
	s.renderMachinePageWithConvertJoinerResult(w, r, convertJoinerResultView{
		BackupDataDir:   resp.GetBackupDataDir(),
		JoinRequestID:   resp.GetJoinRequestId(),
		JoinRequestCode: resp.GetJoinRequestCode(),
	})
}

// convertJoinerResultView is the template-facing shape of a successful
// ConvertStandaloneToJoiner response - the join_request_code is the
// value to relay to the target Colony's Admin for visual comparison
// before they call ApproveJoinRequest.
type convertJoinerResultView struct {
	BackupDataDir   string
	JoinRequestID   string
	JoinRequestCode string
}

// renderMachinePageWithConvertJoinerError/Result re-render the full
// Machine page with every other panel's data freshly fetched, mirroring
// renderMachinePageWithJoinColonyError's own established shape (there is
// no shared "render with one extra field" helper today - each action's
// error/result path re-fetches the whole page, matching handleMachinePage
// itself) - the one new field (ConvertJoinerFormError or
// ConvertJoinerResult) is added on top.
func (s *Server) renderMachinePageWithConvertJoinerError(w http.ResponseWriter, r *http.Request, formErr string) {
	s.renderMachinePageWithConvertJoiner(w, r, formErr, nil)
}

func (s *Server) renderMachinePageWithConvertJoinerResult(w http.ResponseWriter, r *http.Request, result convertJoinerResultView) {
	s.renderMachinePageWithConvertJoiner(w, r, "", &result)
}

func (s *Server) renderMachinePageWithConvertJoiner(w http.ResponseWriter, r *http.Request, formErr string, result *convertJoinerResultView) {
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
		ConvertJoinerFormError:   formErr,
		ConvertJoinerResult:      result,
		ActivePage:               "machine",
	}))
}
