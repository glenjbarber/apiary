package extdiscovery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultMaxResponseBytes bounds how much of a provider's response body
// Apiary will read.
//
// The bound exists because a discovery client reads from a source it
// does not control, and an unbounded read is a way for that source to
// exhaust managerd's memory. 32 MiB is far above what any of the
// documented discovery responses need and far below what would hurt.
const DefaultMaxResponseBytes int64 = 32 << 20

// DefaultTimeout bounds a single provider request.
//
// It is a wall-clock bound on the whole request, not just the read, so a
// provider that accepts the connection and then stalls cannot hold a
// discovery loop open.
const DefaultTimeout = 30 * time.Second

// HTTPClient is the shared outbound HTTP client for discovery
// providers.
//
// It exists because the two providers' requirements are identical and
// because the requirements are the ones that are easy to get wrong:
//
//   - TLS verification is MANDATORY. There is no option to disable it,
//     and there is no field on this struct that could enable it. A
//     private CA is supported by pointing CAFile at a bundle - not by
//     weakening verification.
//
//   - Only GET is possible. The method is not a parameter on any
//     exported function here; get is unexported and the only exported
//     entry point issues GET.
//
//   - Only a fixed set of paths is reachable. The caller supplies a
//     path that must be in the provider's allowlist.
//
//   - Redirects to a different origin are refused.
//
//   - The response body is bounded.
//
//   - The Authorization header is set per request from a caller-supplied
//     token and is never logged.
type HTTPClient struct {
	// BaseURL is the provider's base URL, including any path prefix
	// (Sylve's is "/api").
	BaseURL *url.URL

	// CAFile is an optional path to a PEM bundle of trusted roots. When
	// empty, the system trust store is used.
	//
	// A private CA is configured by pointing this at a bundle. There is
	// deliberately no "skip verification" option: a discovery client
	// that can be told not to check certificates is a discovery client
	// whose observations can be forged, and a forged observation is
	// indistinguishable from a real one.
	CAFile string

	// Timeout bounds one request. Zero means DefaultTimeout.
	Timeout time.Duration

	// MaxResponseBytes bounds one response body. Zero means
	// DefaultMaxResponseBytes.
	MaxResponseBytes int64

	// HTTPClient is the underlying client. When nil, one is built.
	HTTPClient *http.Client
}

// NewHTTPClient builds a client for a provider base URL.
//
// It fails if the URL is not HTTPS. Plaintext is refused rather than
// tolerated, because a provider credential sent over cleartext is
// disclosed to the network, and discovery sends that credential on every
// poll.
func NewHTTPClient(baseURL, caFile string) (*HTTPClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parsing provider base URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("provider base URL must use https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("provider base URL must include a host")
	}

	client := &HTTPClient{
		BaseURL: parsed,
		CAFile:  strings.TrimSpace(caFile),
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if client.CAFile != "" {
		pem, err := os.ReadFile(client.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading provider CA bundle %s: %w", client.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no valid certificates found in %s", client.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	client.HTTPClient = &http.Client{
		Timeout: client.timeout(),
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
		// A redirect to a different origin is refused: it is the
		// cheapest way for a compromised or misconfigured provider to
		// send a credential somewhere else. Redirects within the same
		// origin are allowed, because a provider may legitimately
		// redirect to a canonical path.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			if !sameOrigin(req.URL, via[0].URL) {
				return fmt.Errorf("refusing redirect to a different origin")
			}
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	return client, nil
}

func (c *HTTPClient) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

func (c *HTTPClient) maxBytes() int64 {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return DefaultMaxResponseBytes
}

// sameOrigin reports whether two URLs share scheme and host.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// ProviderError is a failure to reach or read from a provider.
//
// It carries the HTTP status when there was one, because the status is
// what distinguishes an authentication failure from a permission
// failure from a transient server error, and those three need different
// handling and different operator responses.
type ProviderError struct {
	// Status is the HTTP status code, or 0 if the request never
	// produced one.
	Status int

	// Endpoint is the path that was requested. It is a fixed,
	// allowlisted path - never anything derived from a response.
	Endpoint string

	// Err is the underlying cause.
	Err error
}

func (e *ProviderError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: HTTP %d: %v", e.Endpoint, e.Status, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Endpoint, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// StateFor maps an error to the discovery state it implies.
//
// This mapping is the reason the distinction between "absent" and "not
// seen" survives contact with a real HTTP API: a 401 is
// StateUnauthorized and a 403 is StateForbidden, and neither is ever
// mistaken for StateEmpty.
func StateFor(err error) State {
	if err == nil {
		return StateFresh
	}
	var perr *ProviderError
	if !asProviderError(err, &perr) {
		return StateUnavailable
	}
	switch {
	case perr.Status == http.StatusUnauthorized:
		return StateUnauthorized
	case perr.Status == http.StatusForbidden:
		return StateForbidden
	default:
		return StateUnavailable
	}
}

// asProviderError is errors.As specialised to *ProviderError, kept as
// a helper so the import of "errors" stays in one place.
func asProviderError(err error, target **ProviderError) bool {
	for err != nil {
		if pe, ok := err.(*ProviderError); ok {
			*target = pe
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// GetJSON issues a GET against an allowlisted path and decodes the
// response into out.
//
// The path MUST be in allowed. It is a compile-time constant at every
// call site, and the check here is a second, independent barrier rather
// than the only one.
//
// The bearer token is read from tokenFile on each call rather than
// cached, so that rotating the file takes effect without a restart.
func (c *HTTPClient) GetJSON(ctx context.Context, allowed []string, path, tokenFile string, out any) error {
	if !pathAllowed(allowed, path) {
		return &ProviderError{
			Endpoint: path,
			Err:      fmt.Errorf("endpoint is not in the provider's read allowlist"),
		}
	}

	token, err := readToken(tokenFile)
	if err != nil {
		return &ProviderError{Endpoint: path, Err: err}
	}

	target := *c.BaseURL
	target.Path = strings.TrimSuffix(target.Path, "/") + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return &ProviderError{Endpoint: path, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return &ProviderError{Endpoint: path, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The body is NOT included in the error. A provider's error
		// body can echo the request, and the request carried the
		// credential.
		return &ProviderError{
			Status:   resp.StatusCode,
			Endpoint: path,
			Err:      fmt.Errorf("provider returned an error status"),
		}
	}

	limited := io.LimitReader(resp.Body, c.maxBytes()+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return &ProviderError{Status: resp.StatusCode, Endpoint: path, Err: err}
	}
	if int64(len(body)) > c.maxBytes() {
		return &ProviderError{
			Status:   resp.StatusCode,
			Endpoint: path,
			Err:      fmt.Errorf("provider response exceeded %d bytes", c.maxBytes()),
		}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return &ProviderError{Status: resp.StatusCode, Endpoint: path, Err: fmt.Errorf("decoding provider response: %w", err)}
	}
	return nil
}

// pathAllowed reports whether path matches one of the allowed path
// patterns.
//
// A pattern may contain a single "{param}" placeholder, which matches
// exactly one path segment. Everything else must match LITERALLY, and
// the SEGMENT COUNT must match exactly.
//
// The literal requirement is the point. A prefix match would let
// "/cluster" allow "/cluster/join", which is a mutation, and a
// substring match would let "/cluster/status" allow
// "/x/cluster/status". Only a fixed shape with fixed literal
// segments admits a request, so a path that does not exist in the
// provider's API cannot be reached by construction.
func pathAllowed(allowed []string, path string) bool {
	for _, pattern := range allowed {
		if matchPathPattern(pattern, path) {
			return true
		}
	}
	return false
}

// matchPathPattern matches a path against a pattern with at most one
// "{param}" placeholder.
func matchPathPattern(pattern, path string) bool {
	if !strings.Contains(pattern, "{param}") {
		return pattern == path
	}

	prefix, suffix, _ := strings.Cut(pattern, "{param}")
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return false
	}

	// The placeholder must cover exactly one segment: whatever is
	// between the prefix and the suffix must not contain a separator.
	middle := path[len(prefix) : len(path)-len(suffix)]
	if middle == "" {
		return false
	}
	return !strings.Contains(middle, "/")
}

// readToken reads a bearer token from a file.
//
// The file is read fresh each time so that a rotation takes effect
// without restarting managerd. The token is never logged and never
// included in an error message.
func readToken(tokenFile string) (string, error) {
	if strings.TrimSpace(tokenFile) == "" {
		return "", fmt.Errorf("no provider credential file configured")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("reading provider credential file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("provider credential file is empty")
	}
	return token, nil
}
