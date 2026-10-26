package frontend

// ADR-0177's operator surface: the peer trust store, and the
// transition that replaces one of its certificates.
//
// WHY A PAGE. The transition is a THREE-STEP act with a window in the
// middle, and the middle is exactly where an operator needs to look:
// between staging and activation, the Colony is trusting TWO
// certificates for one Comb. An operator who cannot see that cannot
// reason about it.
//
// WHY ITS OWN PAGE, NOT THE CERTIFICATES PAGE. That page is
// deliberately read-only and deliberately ONE Comb's view - its own
// handler says so, because ListOriginCertificates answers only for the
// managerd that receives it and has no peer-forwarding path. The peer
// trust store is the opposite: it is COLONY-WIDE, replicated, and
// every Comb's read of it is identical. Two pages, two honest scopes.
//
// WHY NOT THE MACHINE PAGE'S TLS SECTION. That section configures
// THIS Comb; the trust store is the Colony's view of all of them.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

// trustStoreRow is one pinned Comb on the page.
type trustStoreRow struct {
	// NodeID is the Comb's raft identity - the store's key.
	NodeID string
	// CombName is the operator-facing label, if the pin recorded one.
	CombName string
	// Fingerprint is the CURRENT certificate's digest.
	Fingerprint string
	// Expires is the current certificate's expiry, rendered.
	Expires string
	// ExpiresClass is the badge class for that expiry.
	ExpiresClass string
	// IsVoter says whether the Comb is a raft member.
	IsVoter bool
	// StagedFingerprint is the INCOMING certificate's digest, when a
	// transition is staged. Empty means none is.
	StagedFingerprint string
	// StagedExpires is the incoming certificate's expiry.
	StagedExpires string
	// StagedExpiresClass is the badge class for it.
	StagedExpiresClass string
}

// trustStoreView is the page's data.
type trustStoreView struct {
	// Rows are the pinned Combs, sorted by node ID.
	Rows []trustStoreRow
	// Staged counts the Combs with a transition in flight.
	Staged int
	// Empty is true when the store holds nothing - which is a real
	// state, not a rendering failure.
	Empty bool
	// EmptyNote explains why, when it is empty.
	EmptyNote string
}

// handleTrustStorePage renders the peer trust store.
func (s *Server) handleTrustStorePage(w http.ResponseWriter, r *http.Request) {
	view, err := s.currentTrustStore(r)
	if err != "" {
		s.render(w, "trust_store_page", s.withAuthFields(r, pageData{Error: err, ActivePage: "trust"}))
		return
	}
	s.render(w, "trust_store_page", s.withAuthFields(r, pageData{TrustStore: view, ActivePage: "trust"}))
}

// currentTrustStore reads the store.
//
// It is the managerd's own read, and the store is replicated - so
// every Comb's page shows the same store.
func (s *Server) currentTrustStore(r *http.Request) (trustStoreView, string) {
	var view trustStoreView
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := s.client.ListTrustedPeers(ctx, &rpcpb.ListTrustedPeersRequest{})
	if err != nil {
		return view, err.Error()
	}
	if resp.GetError() != "" {
		return view, resp.GetError()
	}
	now := time.Now()
	for _, peer := range resp.GetPeers() {
		row := trustStoreRow{
			NodeID:      peer.GetNodeId(),
			CombName:    peer.GetCombName(),
			Fingerprint: peer.GetFingerprint(),
			IsVoter:     peer.GetIsVoter(),
		}
		if peer.GetNotAfterUnix() > 0 {
			expires := time.Unix(peer.GetNotAfterUnix(), 0)
			row.Expires = expires.UTC().Format("2006-01-02 15:04:05Z")
			row.ExpiresClass = expiryBadgeClass(expires, now)
		}
		// A STAGED certificate is the case that matters most: it is
		// the one where the Colony is trusting TWO certificates for
		// one Comb.
		if staged := peer.GetNext(); staged != nil {
			view.Staged++
			row.StagedFingerprint = staged.GetFingerprint()
			if staged.GetNotAfterUnix() > 0 {
				expires := time.Unix(staged.GetNotAfterUnix(), 0)
				row.StagedExpires = expires.UTC().Format("2006-01-02 15:04:05Z")
				row.StagedExpiresClass = expiryBadgeClass(expires, now)
			}
		}
		view.Rows = append(view.Rows, row)
	}
	sort.Slice(view.Rows, func(i, j int) bool { return view.Rows[i].NodeID < view.Rows[j].NodeID })
	if len(view.Rows) == 0 {
		view.Empty = true
		view.EmptyNote = "This Colony has no pinned members. A Colony built before the peer trust store existed starts empty, and its members' certificates have to be pinned before peer TLS can trust them - apiaryctl pin-trusted-peers is the command that does it."
	}
	return view, ""
}

// expiryBadgeClass classifies a certificate's remaining lifetime for
// the badge.
//
// It reuses the same rule the Certificates page uses, so the same
// certificate reads the same way in both places.
func expiryBadgeClass(expires, now time.Time) string {
	switch {
	case !expires.After(now):
		return "expired"
	case expires.Before(now.Add(hostcertRenewalWindow)):
		return "soon"
	default:
		return "ok"
	}
}

// hostcertRenewalWindow is how long before expiry a certificate starts
// to read as "soon".
//
// It is the same 30 days the renewal uses, so the page's "soon" and
// the renewal's trigger are the same moment - a certificate that reads
// as expiring on this page is one the renewal is about to act on.
const hostcertRenewalWindow = 30 * 24 * time.Hour

// handleTrustTransition stages, activates or abandons a transition.
//
// The three acts are separate on purpose. Staging adds a second trust
// anchor; activation drops the first; abandonment drops the second.
// Each is its own visible, reasoned act, and each records why.
func (s *Server) handleTrustTransition(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderTrustError(w, r, err.Error())
		return
	}
	nodeID := strings.TrimSpace(r.FormValue("node_id"))
	reason := strings.TrimSpace(r.FormValue("reason"))
	action := strings.TrimSpace(r.FormValue("action"))
	if nodeID == "" {
		s.renderTrustError(w, r, "node_id is required")
		return
	}
	if reason == "" {
		s.renderTrustError(w, r, "a reason is required. Replacing a Comb's certificate changes what the Colony trusts, and an incident review can only answer 'why did this identity change' from the log.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var errMsg string
	switch action {
	case "stage":
		// The incoming certificate is a FILE on this machine, read
		// here and staged by the server.
		path := strings.TrimSpace(r.FormValue("certificate_path"))
		if path == "" {
			s.renderTrustError(w, r, "the path to the incoming certificate is required")
			return
		}
		certPEM, readErr := readCertificateFile(path)
		if readErr != nil {
			s.renderTrustError(w, r, fmt.Sprintf("reading the incoming certificate: %v", readErr))
			return
		}
		info, evalErr := hostcert.Evaluate(certPEM, hostcert.EvaluateOptions{
			Now:                time.Now().UTC(),
			RequireDNSSAN:      true,
			RequireLoopbackSAN: true,
		})
		if evalErr != nil {
			s.renderTrustError(w, r, fmt.Sprintf("the incoming certificate does not pass the gate: %v", evalErr))
			return
		}
		resp, callErr := s.client.TransitionPeerCertificate(ctx, &rpcpb.TransitionPeerCertificateRequest{
			NodeId: nodeID,
			Next: &rpcpb.TrustedPeer{
				NodeId:       nodeID,
				CertPem:      string(certPEM),
				Fingerprint:  info.Fingerprint,
				NotAfterUnix: info.NotAfter.Unix(),
			},
			OverlapSeconds: 3600,
			Reason:         reason,
		})
		if callErr != nil {
			s.renderTrustError(w, r, callErr.Error())
			return
		}
		errMsg = resp.GetError()
	case "activate":
		resp, callErr := s.client.ActivatePeerCertificateTransition(ctx, &rpcpb.ActivatePeerCertificateTransitionRequest{
			NodeId: nodeID,
			Reason: reason,
		})
		if callErr != nil {
			s.renderTrustError(w, r, callErr.Error())
			return
		}
		errMsg = resp.GetError()
	case "abandon":
		resp, callErr := s.client.AbandonPeerCertificateTransition(ctx, &rpcpb.AbandonPeerCertificateTransitionRequest{
			NodeId: nodeID,
			Reason: reason,
		})
		if callErr != nil {
			s.renderTrustError(w, r, callErr.Error())
			return
		}
		errMsg = resp.GetError()
	default:
		s.renderTrustError(w, r, fmt.Sprintf("unknown action %q", action))
		return
	}
	s.renderTrustResult(w, r, errMsg, nodeID, action, reason)
}

// renderTrustError re-renders the page with an error.
func (s *Server) renderTrustError(w http.ResponseWriter, r *http.Request, msg string) {
	view, err := s.currentTrustStore(r)
	if err != "" {
		view = trustStoreView{EmptyNote: err}
	}
	s.render(w, "trust_store_page", s.withAuthFields(r, pageData{Error: msg, TrustStore: view, ActivePage: "trust"}))
}

// renderTrustResult re-renders the page after a transition attempt.
//
// The message says which act was attempted and what became of it,
// because an operator who changed a Comb's trust needs to know whether
// the change took.
func (s *Server) renderTrustResult(w http.ResponseWriter, r *http.Request, errMsg, nodeID, action, reason string) {
	view, err := s.currentTrustStore(r)
	if err != "" {
		view = trustStoreView{EmptyNote: err}
	}
	success := ""
	if errMsg == "" {
		switch action {
		case "stage":
			success = "The incoming certificate is staged. BOTH certificates are trusted until the overlap ends or you activate the new one."
		case "activate":
			success = "The staged certificate is now the trusted one, and the previous certificate is no longer trusted."
		case "abandon":
			success = "The staged certificate was dropped. The Comb keeps the certificate it already had."
		}
	}
	s.render(w, "trust_store_page", s.withAuthFields(r, pageData{
		Error: errMsg, TrustStore: view, ActivePage: "trust",
		TrustSuccess: success, TrustNodeID: nodeID, TrustAction: action, TrustReason: reason,
	}))
}

// readCertificateFile reads a certificate from a path.
//
// The incoming certificate for a transition is a FILE on the machine
// that will serve it - the one a renewal or a prepared-certificate
// adoption produced. It is read here because the operator names where
// it is; the operator does not paste its contents.
func readCertificateFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return data, nil
}
