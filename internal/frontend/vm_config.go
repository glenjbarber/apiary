package frontend

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// handleSetVMConfig is the Edit configuration panel's save (issue #111).
// It changes only vCPU count and RAM, through the narrow SetVMConfig RPC,
// so identity, disks, network and every other field are carried over by
// the FSM rather than re-sent from a possibly stale copy of the page.
//
// Validation happens in two places and both are authoritative for their
// own part: the FSM refuses unsupported ranges and deleting VMs, and the
// manager refuses growth the owning node cannot hold. This handler only
// parses the form, so a non-numeric value gets a plain message here and
// never reaches the RPC.
//
// Success re-renders the page with a restart-required notice. The FSM
// records the change, but a running guest keeps the vCPUs and RAM it
// launched with: the reconciler launches with the stored values only when
// it next starts the VM.
func (s *Server) handleSetVMConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.renderVMPageWith(w, r, id, vmPageForms{Config: "invalid form: " + err.Error()})
		return
	}
	draftCPU := strings.TrimSpace(r.FormValue("vcpus"))
	draftMem := strings.TrimSpace(r.FormValue("memory_mb"))
	refuse := func(msg string) {
		s.renderVMPageWith(w, r, id, vmPageForms{Config: msg, DraftVCPUs: draftCPU, DraftMemoryMB: draftMem})
	}

	vcpus, err := strconv.ParseUint(draftCPU, 10, 32)
	if err != nil {
		refuse("vCPUs must be a whole number, for example 2.")
		return
	}
	memoryMB, err := strconv.ParseUint(draftMem, 10, 64)
	if err != nil {
		refuse("Memory must be a whole number of megabytes, for example 2048.")
		return
	}

	resp, err := s.client.SetVMConfig(r.Context(), &rpcpb.SetVMConfigRequest{
		Id: id, Vcpus: uint32(vcpus), MemoryMb: memoryMB,
	})
	if err != nil {
		refuse(err.Error())
		return
	}
	if resp.GetError() != "" {
		refuse(resp.GetError())
		return
	}
	s.renderVMPageWith(w, r, id, vmPageForms{ConfigNotice: fmt.Sprintf(
		"Saved: %d vCPU and %d MB. Restart required: these settings apply the next time this VM starts. "+
			"A running VM keeps its current vCPUs and memory until it is stopped and started, or restarted.",
		vcpus, memoryMB)})
}
