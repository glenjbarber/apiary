package frontend

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"github.com/glenjbarber/apiary/internal/manager"
)

// sessionCookieName is the cookie holding a logged-in session's token.
const sessionCookieName = "apiary_session"

// sessionTTL bounds how long a session stays valid after login. Fixed
// rather than sliding - simpler, and matches this project's current
// single-developer/local-network stage (re-logging in once a day isn't
// a real burden here).
const sessionTTL = 24 * time.Hour

// sessionInfo is one logged-in session's identity (ADR-0030) - a real
// PAM-authenticated username plus the Role its entry in the operator-
// maintained role-map resolved to, alongside the existing expiry.
type sessionInfo struct {
	expiry   time.Time
	username string
	role     manager.Role
}

// sessionStore tracks valid session tokens in memory. Sessions don't
// survive a frontend restart - matching the same "simple enough for
// this project's current stage" reasoning as BasicAuth before it
// (ADR-0014's consequences); a real deployment needing persistent
// sessions across restarts would need more than this.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]sessionInfo // token -> info
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]sessionInfo)}
}

// Create mints a new session token valid for sessionTTL, for username
// with role (ADR-0030) - both already resolved and confirmed valid by
// the caller (handleLogin) before this is called.
func (s *sessionStore) Create(username string, role manager.Role) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = sessionInfo{expiry: time.Now().Add(sessionTTL), username: username, role: role}
	return token, nil
}

// Valid reports whether token is a live, unexpired session (sweeping
// it out if it has expired) and, if so, its identity.
func (s *sessionStore) Valid(token string) (info sessionInfo, ok bool) {
	if token == "" {
		return sessionInfo{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok = s.sessions[token]
	if !ok {
		return sessionInfo{}, false
	}
	if time.Now().After(info.expiry) {
		delete(s.sessions, token)
		return sessionInfo{}, false
	}
	return info, true
}

// Delete invalidates token (a no-op if it doesn't exist).
func (s *sessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}
