package frontend

import (
	"crypto/subtle"
	"net/http"
)

// BasicAuth wraps next with HTTP Basic Auth, requiring user/pass on
// every request. Comparisons use crypto/subtle so neither credential's
// length or content leaks through response timing. If either user or
// pass is empty, next is returned unwrapped - matching this project's
// current single-developer/local-network stage having no auth by
// default (see CLAUDE.md); cmd/frontend only calls this when both are
// configured.
func BasicAuth(user, pass string, next http.Handler) http.Handler {
	if user == "" || pass == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Apiary"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
