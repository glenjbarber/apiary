package frontend

import (
	"context"
	"net/http"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// resolveSerialLog calls managerd's GetVMSerialLog and folds every
// failure mode (transport error, application error, log not yet
// available) into one message, mirroring resolveConsole's own pattern -
// both handleSerialLogPage and handleSerialLogContent need the same
// content-or-error shape.
func (s *Server) resolveSerialLog(ctx context.Context, id string) (content string, truncated bool, errMsg string) {
	resp, err := s.client.GetVMSerialLog(ctx, &rpcpb.GetVMSerialLogRequest{Id: id})
	if err != nil {
		return "", false, err.Error()
	}
	if resp.GetError() != "" {
		return "", false, resp.GetError()
	}
	if !resp.GetAvailable() {
		return "", false, "this VM has no captured serial log yet (not yet reconciled, or created before serial logging existed)"
	}
	return resp.GetContent(), resp.GetTruncated(), ""
}

// handleSerialLogPage serves the full serial-log page for one VM, with
// the current tail embedded inline (mirroring vms.html's own
// embed-then-poll pattern) so there's no blank flash before the first
// poll fires.
func (s *Server) handleSerialLogPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var vmName string
	if resp, err := s.client.GetVM(r.Context(), &rpcpb.GetVMRequest{Id: id}); err == nil && resp.GetFound() {
		vmName = resp.GetVm().GetName()
	}

	content, truncated, errMsg := s.resolveSerialLog(r.Context(), id)

	s.render(w, "serial_log_page", s.withAuthFields(r, pageData{
		SerialLogVMID:      id,
		SerialLogVMName:    vmName,
		SerialLogContent:   content,
		SerialLogTruncated: truncated,
		SerialLogError:     errMsg,
		ActivePage:         "vms",
	}))
}

// handleSerialLogContent serves just the <pre> fragment, for the page's
// own periodic hx-get refresh - mirroring handleListVMs/vm_rows's
// polling pattern.
func (s *Server) handleSerialLogContent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	content, truncated, errMsg := s.resolveSerialLog(r.Context(), id)

	s.render(w, "serial_log_content", pageData{
		SerialLogContent:   content,
		SerialLogTruncated: truncated,
		SerialLogError:     errMsg,
	})
}
