// Package assumptionregister persists operator-authored environmental claims.
// It is deliberately separate from internal/assumptions: automated
// observations must never silently overwrite a human-owned assertion.
//
// That separation is a boundary this package still enforces after
// evaluation state was added. The two halves of the state model live on
// opposite sides of it: EvidenceStatus and LastVerified are what a
// human (or a future checker) RECORDS, and State is what this package
// DERIVES from them on every read. Nothing that derives a verdict
// writes one back, and nothing derived is ever persisted, so there is
// no stored "true" for an automated observation to overwrite - and no
// stored "true" to rot. TestAutomatedObservationNeverMutatesAnOperatorClaim
// in evaluate_test.go holds that line.
package assumptionregister

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const DefaultPath = "/var/db/apiary/assumption-register.json"

type Claim struct {
	ID        string    `json:"id"`
	Statement string    `json:"statement"`
	Owner     string    `json:"owner"`
	Scope     string    `json:"scope"`
	Evidence  string    `json:"evidence"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// ConsequenceIfFalse states what becomes unsafe, unknown, or
	// unverified when this claim turns out not to hold. The direction
	// this register implements requires it to be recorded; it is not
	// required on save, because the v1 scope's own required list is
	// owner, scope, evidence, timestamps, and an optional verification
	// method, and refusing an operator's existing claim over a field
	// they were never asked for would be a worse failure than showing
	// it empty.
	ConsequenceIfFalse string `json:"consequence_if_false,omitempty"`

	// VerificationMethod records HOW this claim could be re-checked.
	// Nothing in this package runs it - deciding which checks are
	// automated, and what observation backs each claim, is a separate
	// decision this slice deliberately does not make. It is carried so
	// a later checker has somewhere to record what it ran.
	VerificationMethod string `json:"verification_method,omitempty"`

	// EvidenceStatus is the recorded outcome of the last check of this
	// claim's supporting evidence, and LastVerified is when that check
	// happened. Both are raw facts an operator records; neither is
	// this package's opinion. The opinion is Claim.Evaluate's State.
	EvidenceStatus EvidenceStatus `json:"evidence_status,omitempty"`
	LastVerified   time.Time      `json:"last_verified,omitempty"`
}

func (c Claim) Validate() error {
	for name, value := range map[string]string{
		"id": c.ID, "statement": c.Statement, "owner": c.Owner,
		"scope": c.Scope, "evidence": c.Evidence,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("assumption register: %s is required", name)
		}
	}
	if c.ExpiresAt.IsZero() {
		return fmt.Errorf("assumption register: expires_at is required")
	}
	// An unrecognized token is refused here rather than degrading to
	// unknown at read time: a typo would otherwise be a silent
	// downgrade the operator never sees the cause of.
	if c.EvidenceStatus != "" && !knownEvidenceStatuses[c.EvidenceStatus] {
		return fmt.Errorf("assumption register: evidence_status %q is not one of %s", c.EvidenceStatus, knownEvidenceStatusList())
	}
	// "supported" with no verification time is a claim of having
	// checked, with nothing saying when. Evaluate would cap it at
	// unknown anyway; refusing to store it says so at the moment the
	// operator can still fix it.
	if c.EvidenceStatus == EvidenceSupported && c.LastVerified.IsZero() {
		return fmt.Errorf("assumption register: a claim recorded as supported must also record last_verified - a confirmation with no time is not evidence of anything current")
	}
	// A verification recorded at or after the claim's own expiry is
	// incoherent: the claim had already stopped being current.
	if !c.LastVerified.IsZero() && !c.LastVerified.Before(c.ExpiresAt) {
		return fmt.Errorf("assumption register: last_verified (%s) must be before expires_at (%s)", c.LastVerified.UTC().Format(time.RFC3339), c.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// knownEvidenceStatusList renders the accepted evidence_status tokens
// for an error message, in the order they are defined.
func knownEvidenceStatusList() string {
	names := make([]string, 0, len(knownEvidenceStatuses))
	for _, s := range []EvidenceStatus{EvidenceUnobserved, EvidenceSupported, EvidenceContradicted, EvidenceNotApplicable} {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

type Manager struct {
	Path string
	mu   sync.Mutex
}

func (m *Manager) path() string {
	if m.Path == "" {
		return DefaultPath
	}
	return m.Path
}

func (m *Manager) List() ([]Claim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	claims, err := m.loadLocked()
	if err != nil {
		return nil, err
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].ID < claims[j].ID })
	return claims, nil
}

func (m *Manager) Save(claim Claim, now time.Time) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	claims, err := m.loadLocked()
	if err != nil {
		return err
	}
	for i := range claims {
		if claims[i].ID == claim.ID {
			claim.CreatedAt = claims[i].CreatedAt
			claim.UpdatedAt = now
			claims[i] = claim
			return m.writeLocked(claims)
		}
	}
	claim.CreatedAt, claim.UpdatedAt = now, now
	return m.writeLocked(append(claims, claim))
}

func (m *Manager) Delete(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("assumption register: id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	claims, err := m.loadLocked()
	if err != nil {
		return err
	}
	for i := range claims {
		if claims[i].ID == id {
			return m.writeLocked(append(claims[:i], claims[i+1:]...))
		}
	}
	return fmt.Errorf("assumption register: claim %q does not exist", id)
}

func (m *Manager) loadLocked() ([]Claim, error) {
	body, err := os.ReadFile(m.path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var claims []Claim
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, fmt.Errorf("assumption register: parsing %s: %w", m.path(), err)
	}
	return claims, nil
}

func (m *Manager) writeLocked(claims []Claim) error {
	body, err := json.MarshalIndent(claims, "", "  ")
	if err != nil {
		return err
	}
	path := m.path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".assumption-register-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
