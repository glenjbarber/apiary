// Package httpserver builds the http.Server the web-facing daemons
// (frontend, restshimd) listen with.
//
// http.ListenAndServe uses a zero-value Server, which sets no timeouts at
// all: a client can open a connection and never finish sending its headers,
// and the server holds it (and a goroutine and file descriptor) forever.
// Both daemons expose an unauthenticated page (the login form) to anything
// that can reach their port, so that is a cheap way to exhaust them.
package httpserver

import (
	"net/http"
	"time"
)

const (
	// ReadHeaderTimeout bounds how long a client may take to send its request
	// headers. Real browsers and API clients send them immediately.
	ReadHeaderTimeout = 10 * time.Second

	// IdleTimeout bounds how long a keep-alive connection may sit unused
	// between requests.
	IdleTimeout = 2 * time.Minute
)

// New returns a Server for addr and handler with the timeouts above.
//
// It deliberately does NOT set ReadTimeout or WriteTimeout. Those cover the
// whole request or response, so they would cut off ISO uploads (multi-GB
// request bodies), the VNC console WebSocket, and streamed serial-log
// responses, all of which are legitimately long-lived. Header and idle
// timeouts close the slow-connection hole without touching those.
func New(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
		IdleTimeout:       IdleTimeout,
	}
}
