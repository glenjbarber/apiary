package frontend

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// sessionCookieName is the cookie holding a logged-in session's token.
const sessionCookieName = "apiary_session"

// sessionTTL bounds how long a session stays valid after login. Fixed
// rather than sliding - simpler, and matches this project's current
// single-developer/local-network stage (re-logging in once a day isn't
// a real burden here).
const sessionTTL = 24 * time.Hour

// sessionStore tracks valid session tokens in memory. Sessions don't
// survive a frontend restart - matching the same "simple enough for
// this project's current stage" reasoning as BasicAuth before it
// (ADR-0014's consequences); a real deployment needing persistent
// sessions across restarts would need more than this.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]time.Time)}
}

// Create mints a new session token valid for sessionTTL.
func (s *sessionStore) Create() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = time.Now().Add(sessionTTL)
	return token, nil
}

// Valid reports whether token is a live, unexpired session, sweeping it
// out if it has expired.
func (s *sessionStore) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// Delete invalidates token (a no-op if it doesn't exist).
func (s *sessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}
