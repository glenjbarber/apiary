package frontend

import (
	"net/http"
)

// handleJoinGuidedPage keeps the old wizard URL useful while sending
// operators to the actual join procedure. The former wizard only advanced
// client-supplied step names and could claim success without performing a
// join, so it must not remain available as an apparent alternative.
func (s *Server) handleJoinGuidedPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.Redirect(w, r, "/machine#join-colony", http.StatusSeeOther)
}
