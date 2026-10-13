// The standalone-Comb-to-Colony-joiner conversion action (ADR-0105): a
// narrow, Admin-only Machine page action distinct from the "Join a
// Colony" panel in joincolony.go - that panel is for a genuinely fresh
// node with no state of its own; this one is for converting an
// already-bootstrapped standalone Comb, which needs its own explicit
// acknowledgement and irreversible-work guard.
package frontend

import (
	"net/http"
	"net/url"
	"strings"

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
//
// Always ends in a redirect to a GET (issue #46), never a direct render of
// the POST's own response: this action stops and restarts raftd, so a
// browser reload of a directly-rendered result page resubmits the POST and
// restarts raftd again, every time. Carrying the outcome in the query
// string instead (the same pattern as redirectAfterJoinRequestAction and
// handlePreflightJoinRequest) makes a reload idempotent - it re-fetches the
// Machine page and re-reads the same query values rather than converting
// anything a second time.
func (s *Server) handleConvertStandaloneToJoiner(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectAfterConvertToJoiner(w, r, err.Error(), nil)
		return
	}
	resp, err := s.client.ConvertStandaloneToJoiner(r.Context(), &rpcpb.ConvertStandaloneToJoinerRequest{
		TargetManagerdAddress: withFixedPort(r.FormValue("target_managerd_host"), managerdListenerPort),
		RaftBind:              withFixedPort(r.FormValue("raft_bind_host"), raftdListenerPort),
		ConfirmPhrase:         r.FormValue("confirm_phrase"),
	})
	if err != nil {
		s.redirectAfterConvertToJoiner(w, r, err.Error(), nil)
		return
	}
	if resp.GetError() != "" {
		s.redirectAfterConvertToJoiner(w, r, resp.GetError(), nil)
		return
	}
	s.redirectAfterConvertToJoiner(w, r, "", &convertJoinerResultView{
		BackupDataDir:   resp.GetBackupDataDir(),
		JoinRequestID:   resp.GetJoinRequestId(),
		JoinRequestCode: resp.GetJoinRequestCode(),
	})
}

// redirectAfterConvertToJoiner carries the outcome in the query string of
// a GET back to the Machine page - see handleConvertStandaloneToJoiner's
// own doc comment for why this is a redirect rather than a direct render.
func (s *Server) redirectAfterConvertToJoiner(w http.ResponseWriter, r *http.Request, formErr string, result *convertJoinerResultView) {
	if formErr != "" {
		http.Redirect(w, r, "/machine?convert_joiner_error="+url.QueryEscape(formErr), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/machine?convert_joiner_backup_dir="+url.QueryEscape(result.BackupDataDir)+
		"&convert_joiner_request_id="+url.QueryEscape(result.JoinRequestID)+
		"&convert_joiner_request_code="+url.QueryEscape(result.JoinRequestCode), http.StatusFound)
}

// currentConvertJoinerResult reads back what redirectAfterConvertToJoiner
// just put in the query string (the same pattern as currentJoinColonyResult
// in joincolony.go) - a GET, so a reload re-reads the same values instead
// of converting anything again.
func (s *Server) currentConvertJoinerResult(r *http.Request) (result *convertJoinerResultView, formErr string) {
	if errMsg := r.URL.Query().Get("convert_joiner_error"); errMsg != "" {
		return nil, raftdRestartingNotice(errMsg)
	}
	requestID := r.URL.Query().Get("convert_joiner_request_id")
	if requestID == "" {
		return nil, ""
	}
	return &convertJoinerResultView{
		BackupDataDir:   r.URL.Query().Get("convert_joiner_backup_dir"),
		JoinRequestID:   requestID,
		JoinRequestCode: r.URL.Query().Get("convert_joiner_request_code"),
	}, ""
}

// raftdRestartingSocketErrSubstring is the error text a dial against this
// Comb's own raftd socket produces during the brief window
// ConvertStandaloneToJoiner stops and restarts it in (issue #46's observed
// symptom: "dial unix /var/run/apiary/raftd.sock: no such file or
// directory"). Matched as a substring of whatever RPC actually surfaced it
// (ListVMs and any other call that reaches this node's own raftd, such as
// the firewall-pause table's own fetch), not parsed, for the same reason
// explainPeerLinkError in cluster_overview.go matches its own EOF text as
// a substring: this is a dependency's wrapped error, not a typed error
// this package controls.
const raftdRestartingSocketErrSubstring = "raftd.sock"

// raftdRestartingNotice translates the raw socket-dial error any
// raftd-backed status fetch can produce in that same restart window into
// an in-progress notice naming the cause and telling the operator to
// reload shortly, rather than a bare dial error that reads as a crash.
// Every other error is returned unchanged.
func raftdRestartingNotice(errMsg string) string {
	if !strings.Contains(errMsg, raftdRestartingSocketErrSubstring) {
		return errMsg
	}
	return "raftd is restarting as part of converting this Comb to a joiner - this can take a few seconds. Reload this page once it finishes; the original error was: " + errMsg
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
