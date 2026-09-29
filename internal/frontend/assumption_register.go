package frontend

import (
	"net/http"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// claimStateOrder is every state a claim's derived evaluation can be,
// in the order they appear in the page's legend. It is a closed list
// because a UI that renders whatever string arrives is a UI that can
// render a state this build does not understand as though it did - and
// an unrecognized state must read as unknown, not as its own thing.
var claimStateOrder = []string{claimSupported, claimContradicted, claimStale, claimNotApplicable, claimUnknown}

// The five states, named once. The tokens are
// assumptionregister's own, and they are the same five the health card
// uses for the same question, so a reader who has learned one has
// learned both.
const (
	claimSupported     = "supported"
	claimContradicted  = "contradicted"
	claimStale         = "stale"
	claimNotApplicable = "not_applicable"
	claimUnknown       = "unknown"
)

// claimStateShort is the one-word form for the badge. The badge
// carries the colour and the short word; the sentence below it carries
// the meaning. Neither is a bare enum, because the raw token is never
// shown to a reader - it exists on the page as a data-state attribute
// for the stylesheet and for tests, not as something an operator reads.
var claimStateShort = map[string]string{
	claimSupported:     "Supported",
	claimContradicted:  "Contradicted",
	claimStale:         "Stale",
	claimNotApplicable: "Not applicable",
	claimUnknown:       "Unknown",
}

// claimStateWords is the explicit English for each state. The enum
// token alone is never what an operator reads: "supported" in a table
// cell is a bare enum, and this project's rule is that a verdict is
// never conveyed by a colour or by a bare token. The badge carries the
// colour, this carries the meaning, and state_detail carries why.
var claimStateWords = map[string]string{
	claimSupported:     "Supported - evidence confirmed within this claim's own lifetime",
	claimContradicted:  "Contradicted - the last recorded check found the evidence does not hold",
	claimStale:         "Stale - this claim passed its own expiry and is no longer current",
	claimNotApplicable: "Not applicable - recorded as not applying to this environment",
	claimUnknown:       "Unknown - no evidence either way, which is not the same as true",
}

// claimStateBadge maps each state onto the stylesheet's existing badge
// tokens. The class is decoration on top of the words, never the
// message: a state with no colour still reads correctly from the text
// alone, which is what makes the page safe to read with the stylesheet
// suppressed, printed, or scraped.
var claimStateBadge = map[string]string{
	claimSupported:     "ok",
	claimContradicted:  "contradictory",
	claimStale:         "stale",
	claimNotApplicable: "not-applicable",
	claimUnknown:       "unknown",
}

// claimEvidenceStatusOrder is the recorded vocabulary the form offers.
// It is a different list from claimStateOrder on purpose: evidence
// status is what someone wrote down about a check, and state is what
// this build concludes. Offering "stale" as something an operator can
// record would be offering a conclusion as an input.
var claimEvidenceStatusOrder = []string{"unobserved", "supported", "contradicted", "not_applicable"}

// claimEvidenceStatusWords spells out what each recorded token means
// when it is chosen, because the token in a dropdown says nothing to
// somebody reading the form for the first time.
var claimEvidenceStatusWords = map[string]string{
	"unobserved":     "unobserved - nobody has checked this claim's evidence",
	"supported":      "supported - a check confirmed the evidence; requires a last-verified time",
	"contradicted":   "contradicted - a check found the evidence does not hold",
	"not_applicable": "not applicable - this claim does not apply to this environment",
}

// assumptionEvidenceStatusOption is one row of the evidence-status
// dropdown: the token sent to the manager, and the words shown beside
// it so an operator choosing one learns what it asserts.
type assumptionEvidenceStatusOption struct {
	Value string
	Label string
}

// claimEvidenceStatusOptions is the (value, label) pair list the
// template ranges over, built once so the order is defined next to the
// vocabulary rather than in the HTML.
var claimEvidenceStatusOptions = func() []assumptionEvidenceStatusOption {
	out := make([]assumptionEvidenceStatusOption, 0, len(claimEvidenceStatusOrder))
	for _, token := range claimEvidenceStatusOrder {
		label, ok := claimEvidenceStatusWords[token]
		if !ok {
			// Unreachable for the list above, and if it ever is
			// reachable the token is still offered rather than
			// silently missing from the operator's choices.
			label = token
		}
		out = append(out, assumptionEvidenceStatusOption{Value: token, Label: label})
	}
	return out
}()

// claimStateView resolves a state off the wire into the three things
// the page needs for it: the state itself (normalized, so an
// unrecognized token is already unknown by the time it reaches the
// template), the badge class, and the words. The detail is the
// reporting node's own sentence, passed through untouched, with a
// fallback only for a claim that arrived with no evaluation at all.
func claimStateView(state, detail string) (resolved, short, label, badge, why string) {
	if _, known := claimStateWords[state]; !known {
		state = claimUnknown
		if detail == "" {
			detail = "this claim has not been evaluated by any node, so there is no evidence either way"
		}
	}
	if detail == "" {
		detail = claimStateWords[state]
	}
	return state, claimStateShort[state], claimStateWords[state], claimStateBadge[state], detail
}

type assumptionClaimView struct {
	ID                 string
	Statement          string
	Owner              string
	Scope              string
	Evidence           string
	VerificationMethod string
	ConsequenceIfFalse string
	ExpiresAt          string

	// Expired is retained as its own column because an operator needs
	// to see the clock, not just the verdict. It is derived from State
	// rather than recomputed from the expiry, so the two can never
	// disagree about the same claim on the same page.
	Expired bool

	// EvidenceStatus is the RECORDED token, EvidenceStatusLabel the
	// same thing in words. Keeping them apart is what stops a recorded
	// observation from being read as a conclusion.
	EvidenceStatus      string
	EvidenceStatusLabel string

	// LastVerified is the recorded time of the last check, or the words
	// saying it never happened. The zero Unix second is the wire's way
	// of saying "never"; rendering it as 1970 would dress silence up as
	// evidence.
	LastVerified string

	// State, StateShort, StateLabel, StateBadge, and StateDetail are
	// the DERIVED evaluation the reporting node computed on this read.
	// They are never sent back to a save.
	State       string
	StateShort  string
	StateLabel  string
	StateBadge  string
	StateDetail string
}

func fromRPCAssumptionClaim(claim *rpcpb.AssumptionClaim) assumptionClaimView {
	expires := time.Unix(claim.GetExpiresAtUnix(), 0)
	rawState := claim.GetState()
	resolved, short, label, badge, why := claimStateView(rawState, claim.GetStateDetail())
	status := claim.GetEvidenceStatus()
	statusLabel, known := claimEvidenceStatusWords[status]
	if !known {
		// An operator's typo is shown back to them rather than quietly
		// becoming "unobserved" on the page; the manager's Validate
		// refuses to store it in the first place, so reaching here
		// means it came from a peer or an older file.
		if status == "" {
			status, statusLabel = "unobserved", claimEvidenceStatusWords["unobserved"]
		} else {
			statusLabel = status + " - a token this build does not recognize"
		}
	}

	lastVerified := "never verified"
	if v := claim.GetLastVerifiedUnix(); v != 0 {
		lastVerified = time.Unix(v, 0).Local().Format("2006-01-02 15:04 MST")
	}

	return assumptionClaimView{
		ID: claim.GetId(), Statement: claim.GetStatement(), Owner: claim.GetOwner(),
		Scope: claim.GetScope(), Evidence: claim.GetEvidence(),
		VerificationMethod: claim.GetVerificationMethod(),
		ConsequenceIfFalse: claim.GetConsequenceIfFalse(),
		ExpiresAt:          expires.Local().Format("2006-01-02 15:04 MST"),
		// Expired is the clock, read out of the evaluation rather than
		// recomputed from the expiry here, so the column and the state
		// can never disagree about the same claim on the same page.
		Expired:        resolved == claimStale,
		EvidenceStatus: status, EvidenceStatusLabel: statusLabel,
		LastVerified: lastVerified,
		State:        resolved, StateShort: short, StateLabel: label, StateBadge: badge, StateDetail: why,
	}
}

func (s *Server) assumptionRegisterData(r *http.Request, data pageData) pageData {
	// Suggestions are optional: custom scopes and an unavailable membership
	// inventory must not prevent recording a local claim.
	data.Nodes, _ = s.knownNodes(r)
	data.AssumptionEvidenceStatuses = claimEvidenceStatusOptions
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
	// last_verified is optional on the form and validated against the
	// record's own coherence in one place - the manager's Claim.Validate
	// - rather than duplicated here. Parsing is all this layer does: a
	// field that cannot be a time at all is a form mistake, and a field
	// that is a time but does not fit the claim is a record the
	// operator can fix once they are told which rule they broke.
	// Leaving the time unset sends zero, which the manager reads as
	// "never verified" rather than as the epoch.
	var lastVerifiedUnix int64
	if raw := strings.TrimSpace(r.FormValue("last_verified")); raw != "" {
		verifiedAt, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			s.render(w, "assumption_register_page", s.assumptionRegisterData(r, s.withAuthFields(r, pageData{ActivePage: "assumption-register", AssumptionRegisterErr: "Last verified must be RFC 3339, for example 2026-09-29T09:00:00Z, or left blank if the evidence has never been checked."})))
			return
		}
		lastVerifiedUnix = verifiedAt.Unix()
	}
	// An unrecognized evidence_status is sent through unchanged so the
	// manager refuses it with the list of what it does accept, rather
	// than this layer guessing which token was meant.
	claim := &rpcpb.AssumptionClaim{
		Id: strings.TrimSpace(r.FormValue("id")), Statement: strings.TrimSpace(r.FormValue("statement")),
		Owner: strings.TrimSpace(r.FormValue("owner")), Scope: strings.TrimSpace(r.FormValue("scope")),
		Evidence: strings.TrimSpace(r.FormValue("evidence")), VerificationMethod: strings.TrimSpace(r.FormValue("verification_method")),
		ConsequenceIfFalse: strings.TrimSpace(r.FormValue("consequence_if_false")),
		EvidenceStatus:     strings.TrimSpace(r.FormValue("evidence_status")),
		LastVerifiedUnix:   lastVerifiedUnix,
		ExpiresAtUnix:      expiresAt.Unix(),
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
