package managerlink

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

const (
	// DefaultTimeout bounds a single probe. Loopback answers in
	// microseconds; anything that has not answered in two seconds is not
	// going to be the thing restshimd is waiting for.
	DefaultTimeout = 2 * time.Second

	// DefaultCacheTTL is how long one probe's verdict is reused. Only
	// Explain (the per-failed-request path) reads the cache; a wrong TLS
	// setting is not a thing that flips back and forth within seconds, and
	// re-probing on every failed request would add a connection to an
	// endpoint that is already failing for some other reason.
	DefaultCacheTTL = 5 * time.Second

	// retryInterval is how often Verify re-probes an endpoint that is not
	// answering yet.
	retryInterval = 200 * time.Millisecond
)

// Config is everything a Checker needs to ask its question and to say
// precisely what is wrong when the answer is no.
type Config struct {
	// Addr is managerd's address, host:port.
	Addr string

	// UseTLS is the scheme this process is configured to dial with, i.e.
	// the manager_tls setting. It is only ever compared against what the
	// probe finds - never used to pick credentials.
	UseTLS bool

	// CAFile and ServerName are managerd's TLS trust settings, passed
	// through to tlsdial.ManagerTLSConfig unchanged.
	CAFile     string
	ServerName string

	// ProcessName and ConfigPath appear in messages so a failure names
	// the exact file to edit and the exact daemon to restart. ProcessName
	// is optional ("this process" if empty); ConfigPath is not, since
	// "the config file" without a path is not an instruction.
	ProcessName string
	ConfigPath  string

	// Dial, Timeout and CacheTTL are the injectable seams; the zero value
	// of each means the Default* above, and a negative CacheTTL disables
	// caching altogether.
	Dial     DialFunc
	Timeout  time.Duration
	CacheTTL time.Duration
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

func (c Config) cacheTTL() time.Duration {
	switch {
	case c.CacheTTL > 0:
		return c.CacheTTL
	case c.CacheTTL < 0:
		return 0 // negative disables caching, for tests that need every call to re-probe
	default:
		return DefaultCacheTTL
	}
}

func (c Config) process() string {
	if c.ProcessName != "" {
		return c.ProcessName
	}
	return "this process"
}

// Checker holds one process's view of its manager link: the configured
// scheme, the scheme actually observed, and when it was last observed.
// Safe for concurrent use - the startup check, the background watcher and
// every failing REST request all share one Checker.
type Checker struct {
	cfg Config

	mu       sync.Mutex
	observed Scheme
	at       time.Time
}

// New returns a Checker for cfg. It performs no I/O.
func New(cfg Config) *Checker { return &Checker{cfg: cfg} }

// posture is the raw result of one probe: what the endpoint spoke, or why
// nothing could be learned.
type posture struct {
	scheme Scheme
	err    error
}

// observe returns the endpoint's current scheme, probing if the cached
// verdict has aged out. The scheme it records is only ever a fact; what
// to do about it is a comparison against cfg.UseTLS, made by the caller.
func (c *Checker) observe(ctx context.Context) posture {
	c.mu.Lock()
	if c.observed != SchemeUnknown && time.Since(c.at) < c.cfg.cacheTTL() {
		scheme := c.observed
		c.mu.Unlock()
		return posture{scheme: scheme}
	}
	c.mu.Unlock()

	timeout := c.cfg.timeout()
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scheme, err := Detect(pctx, c.cfg.Addr, c.cfg.Dial, timeout)

	c.mu.Lock()
	if scheme != SchemeUnknown {
		// Only a fact is cached. Caching the absence of one would keep
		// reporting "unverified" for the whole TTL after managerd came
		// up, and would make Verify's retry loop pointless - it would
		// keep re-reading its own cache instead of re-probing.
		c.observed, c.at = scheme, time.Now()
	} else {
		c.observed, c.at = SchemeUnknown, time.Time{}
	}
	c.mu.Unlock()
	return posture{scheme: scheme, err: err}
}

// Verify checks the configured scheme against the endpoint's real one,
// and returns one of:
//
//   - nil, if they agree and (when TLS) the trust material verifies;
//   - *MismatchError, if they disagree - the endpoint speaks TLS and the
//     config says plaintext, or the reverse;
//   - *UntrustedError, if both agree on TLS but the certificate does not
//     verify against the configured CA/name;
//   - *UnreachableError / *UndeterminedError, if the answer could not be
//     learned at all.
//
// The last two are "no evidence" outcomes and are deliberately not folded
// into the first two: an endpoint nobody could reach is not a proven
// mismatch, and a startup check that called it one would refuse to start
// a correctly-configured daemon because of a restart race.
//
// grace bounds how long to keep re-probing an endpoint that is not
// answering yet, so managerd starting a moment after restshimd is not
// fatal. A permanent answer comes back immediately - there is nothing to
// wait for.
func (c *Checker) Verify(ctx context.Context, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	var last error
	for {
		err := c.evaluate(ctx)
		if err == nil || IsPermanent(err) {
			return err
		}
		last = err
		if !time.Now().Add(retryInterval).Before(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(retryInterval):
		}
	}
}

// evaluate is one attempt at Verify: a single probe plus, when both sides
// say TLS, one real handshake.
func (c *Checker) evaluate(ctx context.Context) error {
	p := c.observe(ctx)
	switch {
	case p.scheme == SchemeUnknown && p.err != nil && !isTimeout(p.err):
		return &UnreachableError{Addr: c.cfg.Addr, Err: p.err}
	case p.scheme == SchemeUnknown:
		// Either a probe that timed out or one that was answered with
		// nothing usable. The endpoint may well have accepted the
		// connection; what is missing is an answer, so this is not
		// reported as unreachable.
		return &UndeterminedError{Addr: c.cfg.Addr}
	case p.err != nil:
		// A scheme was identified and something else went wrong (a
		// deadline on the write, say). The scheme is still the more
		// useful fact, so keep comparing it; the err is dropped here
		// only because the mismatch is the thing the operator has to fix
		// first. Verify re-probes on its next attempt anyway.
		log.Printf("managerlink: probing %s: %v", c.cfg.Addr, p.err)
	}
	return c.compare(ctx, p.scheme)
}

// isTimeout reports whether a probe failed by running out of time rather
// than by being refused - "nobody answered" and "nothing is listening" are
// different facts, and only the second is evidence about the host.
func isTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}

// compare turns one observed scheme into a verdict against the configured
// one, verifying the certificate when both sides agree on TLS.
func (c *Checker) compare(ctx context.Context, scheme Scheme) error {
	switch {
	case scheme == SchemeTLS && !c.cfg.UseTLS:
		return c.mismatch(SchemePlaintext, SchemeTLS)
	case scheme == SchemePlaintext && c.cfg.UseTLS:
		return c.mismatch(SchemeTLS, SchemePlaintext)
	case scheme == SchemeTLS && c.cfg.UseTLS:
		timeout := c.cfg.timeout()
		tctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := verifyTrust(tctx, c.cfg.Addr, c.cfg.CAFile, c.cfg.ServerName); err != nil {
			return &UntrustedError{Addr: c.cfg.Addr, Err: err, ConfigPath: c.cfg.ConfigPath, ProcessName: c.cfg.ProcessName}
		}
	}
	return nil
}

func (c *Checker) mismatch(configured, detected Scheme) *MismatchError {
	return &MismatchError{
		Addr:        c.cfg.Addr,
		Configured:  configured,
		Detected:    detected,
		ConfigPath:  c.cfg.ConfigPath,
		ProcessName: c.cfg.ProcessName,
	}
}

// permanent marks the failures that are decided rather than undecided. Every
// error this package returns implements it, so the decision is a value and
// not the mere presence of a type - "managerd is not up" must never be
// mistaken for "managerd is misconfigured" by a type assertion alone.
type permanent interface{ isPermanent() bool }

// IsPermanent reports whether err is a failure that retrying cannot fix - a
// scheme disagreement or an untrusted certificate - as opposed to one that
// means the answer is not yet knowable.
//
// The interface is checked for a true return value, not merely for being
// implemented: the "no evidence" failures implement it too, and a check
// that stopped at the type assertion would call a managerd outage
// permanent - the exact inversion this package exists to avoid.
func IsPermanent(err error) bool {
	var p permanent
	return errors.As(err, &p) && p.isPermanent()
}

// serverPrefaceEOF is grpc-go's message when a plaintext client is
// answered by a TLS listener. Quoted in MismatchError.Error because it is
// what the operator is already staring at in the live log, and naming it
// ties the startup error to the 502s it came from.
const serverPrefaceEOF = "error reading server preface: EOF"

// MismatchError says the endpoint speaks a different transport scheme than
// the one configured - the exact failure that otherwise turns into a bare
// 502 for every caller, with nothing in any log pointing at the cause.
type MismatchError struct {
	Addr        string
	Configured  Scheme
	Detected    Scheme
	ConfigPath  string
	ProcessName string
}

func (e *MismatchError) isPermanent() bool { return true }

func (e *MismatchError) Error() string {
	head := fmt.Sprintf("managerd at %s speaks %s on the wire, but %s is configured to dial it as %s",
		e.Addr, e.Detected, e.process(), e.Configured)
	if e.Detected == SchemeTLS && e.Configured == SchemePlaintext {
		// Worth spelling out the consequence, because this is precisely
		// how it presents otherwise: no handshake ever completes, every
		// call dies on "error reading server preface: EOF", and the
		// caller gets a 502 that names none of this.
		return head + fmt.Sprintf(": every call fails with %q and reaches its REST caller as a bare 502. "+
			"Fix: set \"manager_tls\": true in %s (plus \"manager_tls_ca\": \"<ca.pem>\" if managerd's "+
			"certificate is self-signed, and \"manager_tls_server_name\" if that certificate names a host "+
			"other than the one dialed) and restart %s", serverPrefaceEOF, e.ConfigPath, e.process())
	}
	return head + fmt.Sprintf(": no TLS handshake can ever complete against it. "+
		"Fix: set \"manager_tls\": false in %s (or point manager_addr at the port managerd actually "+
		"serves TLS on) and restart %s", e.ConfigPath, e.process())
}

func (e *MismatchError) process() string {
	if e.ProcessName != "" {
		return e.ProcessName
	}
	return "this process"
}

// UntrustedError says both ends agree on TLS, but the certificate does
// not verify - a wrong CA file, a wrong server name, an expired
// certificate. Kept distinct from a mismatch because the scheme is right
// and only the trust is wrong.
type UntrustedError struct {
	Addr        string
	Err         error
	ConfigPath  string
	ProcessName string
}

func (e *UntrustedError) isPermanent() bool { return true }
func (e *UntrustedError) Unwrap() error     { return e.Err }

func (e *UntrustedError) Error() string {
	return fmt.Sprintf("managerd at %s speaks TLS, as configured, but its certificate does not verify: %v. "+
		"Fix: check \"manager_tls_ca\" and \"manager_tls_server_name\" in %s against managerd's own tls_cert",
		e.Addr, e.Err, e.ConfigPath)
}

// UnreachableError says the endpoint could not be reached, so its scheme is
// unknown. Not a mismatch: no evidence either way, and calling it one
// would send the operator to edit a config file that may be correct.
type UnreachableError struct {
	Addr string
	Err  error
}

func (e *UnreachableError) isPermanent() bool { return false }
func (e *UnreachableError) Unwrap() error     { return e.Err }

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("cannot reach managerd at %s (%v), so whether it speaks TLS is unverified - "+
		"that is a reachability problem (managerd not started yet, or a PF rule), not a TLS setting",
		e.Addr, e.Err)
}

// UndeterminedError says the endpoint was reached (or the probe ran out
// of time) without answering in a way that identifies its transport -
// neither an HTTP/2 response nor a TLS record. No evidence either way, and
// deliberately not reported as a mismatch, because "the answer is not yet
// knowable" and "the config is wrong" lead to different actions.
type UndeterminedError struct {
	Addr string
}

func (e *UndeterminedError) isPermanent() bool { return false }

func (e *UndeterminedError) Error() string {
	return fmt.Sprintf("managerd at %s did not answer in a way that identifies its transport within the probe "+
		"timeout, so its TLS posture is unverified - refusing to assume either scheme", e.Addr)
}

// Diagnosis is the named, actionable account of one failed call to
// managerd, for a caller that has to tell a transient outage apart from a
// permanent misconfiguration.
type Diagnosis struct {
	// Class is a stable machine-readable token, safe to alert on.
	Class string

	// Detail is the same human explanation the startup check prints,
	// naming the cause and the fix.
	Detail string

	// Permanent is true for the misconfigurations - a caller must not
	// retry those, because retrying cannot help.
	Permanent bool
}

// Class tokens, stable enough for a caller's alerting to match on.
const (
	// ClassSchemeMismatch is a permanent configuration error: the
	// configured manager_tls does not match what managerd speaks.
	ClassSchemeMismatch = "manager_tls_scheme_mismatch"

	// ClassTLSUntrusted is a permanent configuration error: the scheme
	// matches but the certificate does not verify.
	ClassTLSUntrusted = "manager_tls_untrusted"

	// ClassUnreachable is a transient failure: managerd could not be
	// reached, and its TLS posture is unverified. Explicitly not
	// "broken configuration" - the config may well be correct.
	ClassUnreachable = "manager_unreachable"

	// ClassCallFailed is the honest default: the link is up, the scheme
	// matched, and the call still failed. The raw error is carried in
	// Detail, so this is never a way of hiding anything.
	ClassCallFailed = "manager_call_failed"
)

// Explain classifies a failed call to managerd. Unlike Verify, which runs
// once at startup, this runs per failed request - so it re-probes the
// endpoint (bounded by Config.Timeout, cached per Config.CacheTTL) rather
// than trusting a startup verdict that may be minutes old.
//
// It reports what it observed, never what would be convenient. A probe
// that cannot reach managerd yields ClassUnreachable, not a scheme
// mismatch: "managerd is down" and "managerd is misconfigured" are
// different problems, and an operator sent to fix the wrong one loses an
// outage.
func (c *Checker) Explain(rpcErr error) Diagnosis {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.timeout())
	defer cancel()

	switch p := c.observe(ctx); p.scheme {
	case SchemeUnknown:
		detail := "managerd's TLS posture is unverified: the endpoint could not be reached at all"
		if p.err != nil {
			detail = fmt.Sprintf("managerd's TLS posture is unverified: %v", p.err)
		}
		return Diagnosis{Class: ClassUnreachable, Detail: detail}
	case SchemeTLS:
		if !c.cfg.UseTLS {
			err := c.mismatch(SchemePlaintext, SchemeTLS)
			return Diagnosis{Class: ClassSchemeMismatch, Permanent: true, Detail: err.Error()}
		}
		var untrusted *UntrustedError
		if err := c.compare(ctx, SchemeTLS); errors.As(err, &untrusted) {
			return Diagnosis{Class: ClassTLSUntrusted, Permanent: true, Detail: untrusted.Error()}
		}
	case SchemePlaintext:
		if c.cfg.UseTLS {
			err := c.mismatch(SchemeTLS, SchemePlaintext)
			return Diagnosis{Class: ClassSchemeMismatch, Permanent: true, Detail: err.Error()}
		}
	}
	return Diagnosis{
		Class:  ClassCallFailed,
		Detail: fmt.Sprintf("the manager link is up and the scheme matches; the call itself failed: %v", rpcErr),
	}
}

// Watch re-verifies the link on an interval and logs every *change* of
// verdict, so a misconfiguration introduced after startup (managerd
// restarted with TLS turned on, say) is noticed and named in the log
// rather than surfacing only as a stream of anonymous 502s. Log-only by
// design: a mismatch discovered now is a config file that needs editing,
// and killing the process from a timer goroutine is not a better answer
// than saying so loudly, once, and letting the per-request diagnosis in
// Explain say it to every caller.
//
// logf defaults to log.Printf; tests pass their own to assert on it.
func (c *Checker) Watch(ctx context.Context, every time.Duration, logf func(format string, args ...any)) {
	if logf == nil {
		logf = log.Printf
	}
	if every <= 0 {
		every = 30 * time.Second
	}
	// Seed from the startup verdict, so the first tick only logs if
	// something actually changed since restshimd checked at boot.
	prev := errText(c.Verify(ctx, 0))
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if current := errText(c.Verify(ctx, 0)); current != prev {
			logf("restshim link: %s", current)
			prev = current
		}
	}
}

func errText(err error) string {
	if err == nil {
		return "manager link verified: the configured manager_tls setting matches what managerd speaks"
	}
	return err.Error()
}
