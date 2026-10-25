package main

import (
	"context"
	"sync"
	"time"

	"github.com/glenjbarber/apiary/internal/frontendconfig"
)

// The LIVE API key (issue #75).
//
// The problem this solves, precisely.
//
// A frontend's manager_api_key is read ONCE, at startup, and attached
// to the gRPC connection as a per-RPC credential. That is fine until
// the moment an operator creates the first API key on the API Keys
// page: the FSM flips auth_enabled on (ADR-0023, permanent and
// colony-wide) at that instant, and this frontend - which had no
// manager_api_key configured, because there was no key to configure
// one with - starts failing every call with Unauthenticated.
//
// The API Keys page already handles the FILE half of this: it installs
// the newly minted key into this Comb's own frontend.json and
// managerd.json the moment the key exists (see
// internal/frontend's provisionSelfAfterFirstKey). But writing the
// file is only half of it. The running frontend's credential was
// captured at startup and never changes, so the file the page just
// wrote is not the credential the frontend is USING. Every call after
// the key was created fails exactly as before, until an operator
// restarts the frontend by hand.
//
// Which is the lockout the issue describes, and it is the only part of
// it that cannot be fixed by writing a file.
//
// So the credential is resolved at CALL TIME instead of captured at
// startup. gRPC calls PerRPCCredentials.GetRequestMetadata once per
// RPC, so a credential that reads live state picks up a key written
// moments earlier with no restart at all.
//
// Why it is a FILE rather than a notification:
//
//   - The key is already in the file. The API Keys page wrote it there.
//     Re-reading it is reading the authoritative value, not
//     reimplementing it.
//
//   - A frontend can also be given a key by hand - an operator editing
//     frontend.json directly, or a join flow installing one. A
//     file-backed credential picks that up too, with no separate
//     mechanism to keep in step.
//
//   - There is no signal to notify on. The page that writes the file is
//     a different process from the frontend that needs to hear about
//     it, and a poll of a file that already holds the answer needs no
//     protocol between them.
//
// The cost is a file read per RPC, which is why the read is cached
// with a short TTL rather than done on every call: the key changes
// about once in a Comb's lifetime, and reading a small file is not
// free on a path that runs for every request.

// liveKeyTTL is how long a cached key is trusted before the file is
// read again.
//
// It is short because the window in which it matters is exactly the
// window an operator is staring at a broken page: the key was just
// created, and the frontend is failing every call. A second of
// staleness there is invisible; a minute of it is a page that looks
// broken for a minute after it was fixed.
//
// It is not zero because a file read per RPC is a real cost on a
// frontend that serves a whole Colony's UI, and the value changes so
// rarely that re-reading it constantly buys nothing.
const liveKeyTTL = time.Second

// liveKey is a credential that resolves the API key at call time.
//
// The zero value is ready to use: it reads the default frontend.json
// path and caches what it finds.
type liveKey struct {
	// path is the frontend.json to read. Empty means the default.
	path string

	// mu guards the cache below.
	mu sync.Mutex
	// cached is the last key read from the file.
	cached string
	// readAt is when the cache was last filled.
	readAt time.Time
	// hasRead records whether the cache has ever been filled, so an
	// unread file and a file that has been read and held nothing are
	// distinguishable.
	hasRead bool
}

// newLiveKey returns a liveKey reading the given path, or the default
// frontend.json path when path is empty.
func newLiveKey(path string) *liveKey {
	return &liveKey{path: path}
}

// key returns the API key to present, reading the file if the cache is
// older than liveKeyTTL.
//
// A file that cannot be read yields an empty key, which is the same
// state the frontend was in before it had one at all: the connection
// carries no credential and managerd's own interceptor decides what
// that means. Failing the RPC outright here would be worse - it would
// turn a frontend that was working (because auth is not yet enabled)
// into one that cannot make any call at all.
func (k *liveKey) key() string {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.hasRead && time.Since(k.readAt) < liveKeyTTL {
		return k.cached
	}
	cfg, err := (&frontendconfig.Manager{Path: k.path}).Load()
	if err != nil {
		// Keep whatever was cached before. A transient read failure -
		// a half-written file during an atomic replace, say - must not
		// silently strip a working credential from the connection and
		// lock the operator out.
		return k.cached
	}
	k.cached = cfg.ManagerAPIKey
	k.hasRead = true
	k.readAt = time.Now()
	return k.cached
}

// GetRequestMetadata implements grpc.PerRPCCredentials. It is called
// once per RPC, which is what makes the live read possible.
func (k *liveKey) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	key := k.key()
	if key == "" {
		// No credential to present. Returning an empty metadata map
		// rather than an error is deliberate: gRPC sends the call
		// without an authorization header, and managerd's interceptor
		// then applies its own rule - which is "allow it" while auth
		// is not yet enabled, and "refuse it" once it is. Returning
		// an error here would instead fail every call locally, with
		// a message about the frontend's own configuration rather
		// than about authentication.
		return map[string]string{}, nil
	}
	return map[string]string{"authorization": "Bearer " + key}, nil
}

// RequireTransportSecurity is false to match this project's existing
// local-network transport - the key travels in plaintext the same way
// everything else on this connection already does.
//
// This is a credential for a LOCAL connection to this Comb's own
// managerd. The key is not sent to any other host, and the transport
// it travels over is the one the operator configured.
func (*liveKey) RequireTransportSecurity() bool { return false }
