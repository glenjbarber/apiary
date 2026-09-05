package frontend

import (
	"net/http"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

type assumptionClaimView struct {
	ID                 string
	Statement          string
	Owner              string
	Scope              string
	Evidence           string
	VerificationMethod string
	ExpiresAt          string
	Expired            bool
}

func fromRPCAssumptionClaim(claim *rpcpb.AssumptionClaim) assumptionClaimView {
	expires := time.Unix(claim.GetExpiresAtUnix(), 0)
	return assumptionClaimView{
		ID: claim.GetId(), Statement: claim.GetStatement(), Owner: claim.GetOwner(),
		Scope: claim.GetScope(), Evidence: claim.GetEvidence(),
		VerificationMethod: claim.GetVerificationMethod(),
		ExpiresAt:          expires.Local().Format("2006-01-02 15:04 MST"),
		Expired:            !expires.After(time.Now()),
	}
}

func (s *Server) assumptionRegisterData(r *http.Request, data pageData) pageData {
	resp, err := s.client.ListAssumptionClaims(r.Context(), &rpcpb.ListAssumptionClaimsRequest{})
	if err != nil {
		data.AssumptionRegisterErr = err.Error()
		return data
	}
	if resp.GetError() != "" {
		data.AssumptionRegisterErr = resp.GetError()
		return data
	}
	for _, claim := range resp.GetClaims() {
		data.AssumptionClaims = append(data.AssumptionClaims, fromRPCAssumptionClaim(claim))
	}
	return data
}

func (s *Server) handleAssumptionRegisterPage(w http.ResponseWriter, r *http.Request) {
	data := s.assumptionRegisterData(r, s.withAuthFields(r, pageData{ActivePage: "assumption-register"}))
	s.render(w, "assumption_register_page", data)
}

func (s *Server) handleSaveAssumptionClaim(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "assumption_register_page", s.assumptionRegisterData(r, s.withAuthFields(r, pageData{ActivePage: "assumption-register", AssumptionRegisterErr: err.Error()})))
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(r.FormValue("expires_at")))
	if err != nil {
		s.render(w, "assumption_register_page", s.assumptionRegisterData(r, s.withAuthFields(r, pageData{ActivePage: "assumption-register", AssumptionRegisterErr: "Expiration must be RFC 3339, for example 2026-12-31T23:59:59Z."})))
		return
	}
	claim := &rpcpb.AssumptionClaim{
		Id: strings.TrimSpace(r.FormValue("id")), Statement: strings.TrimSpace(r.FormValue("statement")),
		Owner: strings.TrimSpace(r.FormValue("owner")), Scope: strings.TrimSpace(r.FormValue("scope")),
		Evidence: strings.TrimSpace(r.FormValue("evidence")), VerificationMethod: strings.TrimSpace(r.FormValue("verification_method")),
		ExpiresAtUnix: expiresAt.Unix(),
	}
	resp, rpcErr := s.client.SaveAssumptionClaim(r.Context(), &rpcpb.SaveAssumptionClaimRequest{Claim: claim})
	data := s.withAuthFields(r, pageData{ActivePage: "assumption-register"})
	if rpcErr != nil {
		data.AssumptionRegisterErr = rpcErr.Error()
	} else if resp.GetError() != "" {
		data.AssumptionRegisterErr = resp.GetError()
	} else {
		data.AssumptionRegisterOK = "Saved claim " + claim.GetId() + "."
	}
	s.render(w, "assumption_register_page", s.assumptionRegisterData(r, data))
}

func (s *Server) handleDeleteAssumptionClaim(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.DeleteAssumptionClaim(r.Context(), &rpcpb.DeleteAssumptionClaimRequest{Id: r.PathValue("id")})
	data := s.withAuthFields(r, pageData{ActivePage: "assumption-register"})
	if err != nil {
		data.AssumptionRegisterErr = err.Error()
	} else if resp.GetError() != "" {
		data.AssumptionRegisterErr = resp.GetError()
	} else {
		data.AssumptionRegisterOK = "Deleted claim " + r.PathValue("id") + "."
	}
	s.render(w, "assumption_register_page", s.assumptionRegisterData(r, data))
}
