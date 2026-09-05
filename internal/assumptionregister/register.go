// Package assumptionregister persists operator-authored environmental claims.
// It is deliberately separate from internal/assumptions: automated
// observations must never silently overwrite a human-owned assertion.
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
	ID                 string    `json:"id"`
	Statement          string    `json:"statement"`
	Owner              string    `json:"owner"`
	Scope              string    `json:"scope"`
	Evidence           string    `json:"evidence"`
	VerificationMethod string    `json:"verification_method,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
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
	return nil
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
