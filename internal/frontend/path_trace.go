package frontend

import (
	"net/http"
	"strconv"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

type pathTraceStepView struct {
	Stage       string
	Status      string
	StatusClass string
	Summary     string
	Evidence    string
	Explanation string
}

type pathTraceView struct {
	Cell        vmView
	Network     networkView
	Status      string
	StatusClass string
	Summary     string
	Steps       []pathTraceStepView
	NonAtomic   bool
	ActiveProbe bool
}

// handleTracePage serves ADR-0058's bookmarkable, read-only path
// explanation. All multi-Hive evidence gathering stays behind the
// manager RPC so the frontend cannot accidentally label local state as
// belonging to the selected Cell's owner.
func (s *Server) handleTracePage(w http.ResponseWriter, r *http.Request) {
	cells, listErr := s.currentVMs(r, "id", "asc")
	cellID := strings.TrimSpace(r.URL.Query().Get("cell_id"))
	destination := strings.TrimSpace(r.URL.Query().Get("destination"))
	protocol := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("protocol")))
	portText := strings.TrimSpace(r.URL.Query().Get("port"))
	requested := cellID != "" || destination != "" || protocol != "" || portText != ""

	traceErr := listErr
	var result pathTraceView
	if requested && traceErr == "" {
		switch {
		case cellID == "":
			traceErr = "Choose a Cell to trace."
		case destination == "":
			traceErr = "Enter a destination host or IPv4 address."
		default:
			var port uint64
			var err error
			if portText != "" {
				port, err = strconv.ParseUint(portText, 10, 16)
				if err != nil {
					traceErr = "Destination port must be a number from 1 to 65535."
				} else if port == 0 {
					traceErr = "Destination port must be a number from 1 to 65535."
				}
			}
			if traceErr == "" {
				resp, callErr := s.client.TraceCellPath(r.Context(), &rpcpb.TraceCellPathRequest{
					CellId: cellID, Destination: destination,
					Protocol: protocol, Port: uint32(port),
				})
				switch {
				case callErr != nil:
					traceErr = callErr.Error()
				case resp == nil:
					traceErr = "The path trace returned no result."
				case resp.GetError() != "":
					traceErr = resp.GetError()
				default:
					result = fromRPCPathTrace(resp)
				}
			}
		}
	}

	s.render(w, "trace_page", s.withAuthFields(r, pageData{
		TraceCells: cells, TraceRequested: requested,
		TraceCellID: cellID, TraceDestination: destination,
		TraceProtocol: protocol, TracePort: portText,
		TraceError: traceErr, TraceResult: result, ActivePage: "trace",
	}))
}

func fromRPCPathTrace(resp *rpcpb.TraceCellPathResponse) pathTraceView {
	status, class := pathTraceStatusView(resp.GetStatus())
	view := pathTraceView{
		Cell: fromRPCVM(resp.GetCell()), Network: fromRPCNetwork(resp.GetNetwork()),
		Status: status, StatusClass: class, Summary: resp.GetSummary(),
		NonAtomic: resp.GetNonAtomic(), ActiveProbe: resp.GetActiveProbe(),
	}
	for _, step := range resp.GetSteps() {
		stepStatus, stepClass := pathTraceStatusView(step.GetStatus())
		view.Steps = append(view.Steps, pathTraceStepView{
			Stage: step.GetStage(), Status: stepStatus,
			StatusClass: stepClass, Summary: step.GetSummary(),
			Evidence: step.GetEvidence(), Explanation: step.GetExplanation(),
		})
	}
	return view
}

func pathTraceStatusView(status rpcpb.PathTraceStatus) (string, string) {
	switch status {
	case rpcpb.PathTraceStatus_PATH_TRACE_STATUS_CLEAR:
		return "clear", "clear"
	case rpcpb.PathTraceStatus_PATH_TRACE_STATUS_BLOCKED:
		return "blocked", "blocked"
	case rpcpb.PathTraceStatus_PATH_TRACE_STATUS_NOT_APPLICABLE:
		return "not applicable", "not-applicable"
	default:
		return "unknown", "unknown"
	}
}
