// Package cloudflare implements Cloudflare Tunnel exposure v1
// (ADR-0063): reconciling which Cells are exposed into a Hive's own
// pre-provisioned cloudflared Tunnel ingress config, managing that
// Tunnel process's lifecycle, and calling Cloudflare's DNS API to
// create/update just the CNAME record per exposed Cell. This is real
// I/O - an outbound third-party HTTPS API client and a local process
// manager - mirroring internal/hast/internal/pf's role (wraps an
// external tool/API), not internal/invariant's pure-computation role.
//
// The operator pre-provisions one Cloudflare Tunnel per Hive by hand
// (cloudflared tunnel create) - this package never calls Cloudflare's
// Tunnel-provisioning API, only its DNS API, and needs only a narrow
// Zone:DNS:Edit token. See ADR-0063 for the full design and the
// review findings that shaped it.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// apiBaseURL is Cloudflare's real API base - overridable in tests via
// baseURL below, mirroring the established "swap the real backend for
// an httptest.Server" pattern this project already uses elsewhere.
const apiBaseURL = "https://api.cloudflare.com/client/v4"

var baseURL = apiBaseURL

// cloudflareHTTPClient deliberately has its own bounds rather than using
// http.DefaultClient. The caller's context still supplies cancellation and
// any earlier deadline; these limits protect reconciliation when a caller
// accidentally supplies a long-lived context or an endpoint stops responding.
var cloudflareHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&http.Transport{}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	},
}

const maxCloudflareResponseBody = 1 << 20 // 1 MiB

// SetBaseURLForTest overrides the Cloudflare API base URL for the
// duration of a test, returning a func that restores it. It exists so
// that tests of packages that call into this one (internal/acme, whose
// DNS-01 challenge publishing calls CreateTXT/DeleteTXT) can point this
// package at a local httptest.Server without this package exporting its
// HTTP client or transport. Production code must never call this.
func SetBaseURLForTest(url string) (reset func()) {
	old := baseURL
	baseURL = url
	return func() { baseURL = old }
}

type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiResponse struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

// apiErrorResult wraps Cloudflare's own error code so a caller (only
// DeleteCNAME today, to special-case "record not found") can inspect
// it via errors.As without parsing message text.
type apiErrorResult struct {
	Code    int
	Message string
}

func (e *apiErrorResult) Error() string {
	return fmt.Sprintf("cloudflare API error %d: %s", e.Code, e.Message)
}

func doRequest(ctx context.Context, method, path, token string, body any) (*apiResponse, error) {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := cloudflareHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling cloudflare API: %w", err)
	}
	defer resp.Body.Close()

	// Read a bounded amount before decoding. This prevents a broken or
	// compromised endpoint from making JSON decoding allocate without limit.
	encoded, err := io.ReadAll(io.LimitReader(resp.Body, maxCloudflareResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading cloudflare API response (status %d): %w", resp.StatusCode, err)
	}
	if len(encoded) > maxCloudflareResponseBody {
		return nil, fmt.Errorf("reading cloudflare API response (status %d): response body exceeds %d bytes", resp.StatusCode, maxCloudflareResponseBody)
	}

	var parsed apiResponse
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		return nil, fmt.Errorf("decoding cloudflare API response (status %d): %w", resp.StatusCode, err)
	}
	if !parsed.Success {
		if len(parsed.Errors) > 0 {
			return &parsed, &apiErrorResult{Code: parsed.Errors[0].Code, Message: parsed.Errors[0].Message}
		}
		return &parsed, fmt.Errorf("cloudflare API request failed with status %d and no error detail", resp.StatusCode)
	}
	return &parsed, nil
}

// notFoundErrorCode is Cloudflare's own error code for "record does
// not exist" on a DELETE of an already-gone DNS record - treated as
// success, mirroring internal/pf.Manager.Flush's own "No such anchor"
// idempotency fix (ADR-0022's follow-up).
const notFoundErrorCode = 81044

func findRecord(ctx context.Context, token, zoneID, hostname string) (*dnsRecord, error) {
	path := fmt.Sprintf("/zones/%s/dns_records?type=CNAME&name=%s", url.PathEscape(zoneID), url.QueryEscape(hostname))
	resp, err := doRequest(ctx, http.MethodGet, path, token, nil)
	if err != nil {
		return nil, err
	}
	var records []dnsRecord
	if err := json.Unmarshal(resp.Result, &records); err != nil {
		return nil, fmt.Errorf("decoding dns_records list: %w", err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	return &records[0], nil
}

// EnsureCNAME idempotently creates or updates a CNAME record pointing
// hostname at target (a pre-provisioned Tunnel's own
// "<tunnel-id>.cfargotunnel.com" routing target - see ADR-0063).
// Callers must only invoke this when the desired (hostname, target)
// pair actually differs from what was last successfully applied
// (tracked by internal/cluster's own sidecar) - Cloudflare's API is
// neither free nor unlimited, unlike the local CLI tools every other
// "call unconditionally" precedent in this codebase wraps.
func EnsureCNAME(ctx context.Context, token, zoneID, hostname, target string) error {
	existing, err := findRecord(ctx, token, zoneID, hostname)
	if err != nil {
		return fmt.Errorf("looking up existing record for %s: %w", hostname, err)
	}
	rec := dnsRecord{Type: "CNAME", Name: hostname, Content: target, Proxied: true, TTL: 1}
	if existing == nil {
		path := fmt.Sprintf("/zones/%s/dns_records", url.PathEscape(zoneID))
		if _, err := doRequest(ctx, http.MethodPost, path, token, rec); err != nil {
			return fmt.Errorf("creating CNAME for %s: %w", hostname, err)
		}
		return nil
	}
	if existing.Content == target && existing.Proxied {
		return nil
	}
	path := fmt.Sprintf("/zones/%s/dns_records/%s", url.PathEscape(zoneID), url.PathEscape(existing.ID))
	if _, err := doRequest(ctx, http.MethodPut, path, token, rec); err != nil {
		return fmt.Errorf("updating CNAME for %s: %w", hostname, err)
	}
	return nil
}

// CreateTXT creates a new TXT record and returns its Cloudflare record
// ID. Used by ACME's DNS-01 challenge (ADR-0133, internal/acme) to
// publish a challenge value under _acme-challenge.<hostname>.
func CreateTXT(ctx context.Context, token, zoneID, name, value string) (string, error) {
	path := fmt.Sprintf("/zones/%s/dns_records", url.PathEscape(zoneID))
	resp, err := doRequest(ctx, http.MethodPost, path, token, dnsRecord{Type: "TXT", Name: name, Content: value, TTL: 60})
	if err != nil {
		return "", fmt.Errorf("creating TXT record for %s: %w", name, err)
	}
	var created dnsRecord
	if err := json.Unmarshal(resp.Result, &created); err != nil {
		return "", fmt.Errorf("decoding created TXT record for %s: %w", name, err)
	}
	return created.ID, nil
}

// DeleteTXT idempotently removes a TXT record by its Cloudflare record
// ID, mirroring DeleteCNAME's not-found-is-success posture. An empty
// recordID is a no-op, not an error, so a cleanup path that never
// learned an ID (because creation itself failed) can call this
// unconditionally.
func DeleteTXT(ctx context.Context, token, zoneID, recordID string) error {
	if recordID == "" {
		return nil
	}
	path := fmt.Sprintf("/zones/%s/dns_records/%s", url.PathEscape(zoneID), url.PathEscape(recordID))
	if _, err := doRequest(ctx, http.MethodDelete, path, token, nil); err != nil {
		var apiErr *apiErrorResult
		if errors.As(err, &apiErr) && apiErr.Code == notFoundErrorCode {
			return nil
		}
		return fmt.Errorf("deleting TXT record %s: %w", recordID, err)
	}
	return nil
}

// DeleteCNAME idempotently removes hostname's CNAME record.
func DeleteCNAME(ctx context.Context, token, zoneID, hostname string) error {
	existing, err := findRecord(ctx, token, zoneID, hostname)
	if err != nil {
		return fmt.Errorf("looking up existing record for %s: %w", hostname, err)
	}
	if existing == nil {
		return nil
	}
	path := fmt.Sprintf("/zones/%s/dns_records/%s", url.PathEscape(zoneID), url.PathEscape(existing.ID))
	if _, err := doRequest(ctx, http.MethodDelete, path, token, nil); err != nil {
		var apiErr *apiErrorResult
		if errors.As(err, &apiErr) && apiErr.Code == notFoundErrorCode {
			return nil
		}
		return fmt.Errorf("deleting CNAME for %s: %w", hostname, err)
	}
	return nil
}

// Zone is one Cloudflare zone, as this package needs to describe it.
//
// The account is included because a zone name is not unique across
// Cloudflare accounts: the same domain can exist in two accounts, and
// resolving a name to a zone without saying which account would let
// an operator pick a zone they did not mean to.
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Paused bool   `json:"paused"`
	// AccountName is the account that owns the zone, when Cloudflare
	// reports it. It is descriptive, never a routing or
	// authorization input.
	AccountName string `json:"account_name"`
}

// ListZones returns the zones the token can see.
//
// A token scoped to a single zone sees only that zone, which is the
// point: an operator who made a zone-scoped token gets a list of one
// rather than a list of everything their account holds.
//
// The result is sorted by zone name, because this is rendered to an
// operator who has to choose, and a list that reorders itself between
// runs is a list they cannot navigate.
func ListZones(ctx context.Context, token string) ([]Zone, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("cloudflare: the API token is empty")
	}
	resp, err := doRequest(ctx, http.MethodGet, "/zones?per_page=50", token, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: listing zones: %w", err)
	}
	var zones []Zone
	if err := json.Unmarshal(resp.Result, &zones); err != nil {
		return nil, fmt.Errorf("cloudflare: decoding the zone list: %w", err)
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })
	return zones, nil
}

// FindZoneByName returns the zone with the given name.
//
// It refuses an AMBIGUOUS name rather than picking one. Two accounts
// can hold the same domain, and choosing the first would silently
// issue a certificate against a zone the operator did not mean. The
// refusal names both candidates so the operator can disambiguate with
// a zone ID.
func FindZoneByName(ctx context.Context, token, name string) (Zone, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Zone{}, fmt.Errorf("cloudflare: the zone name is empty")
	}
	zones, err := ListZones(ctx, token)
	if err != nil {
		return Zone{}, err
	}
	var matches []Zone
	for _, z := range zones {
		if strings.EqualFold(z.Name, name) {
			matches = append(matches, z)
		}
	}
	switch len(matches) {
	case 0:
		return Zone{}, fmt.Errorf("cloudflare: no zone named %q is visible to this token; a zone-scoped token sees only its own zone", name)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, z := range matches {
			ids = append(ids, z.ID)
		}
		return Zone{}, fmt.Errorf("cloudflare: %q is visible in more than one account (%s); name the zone ID instead", name, strings.Join(ids, ", "))
	}
}

// TokenCheck reports what a token may do.
//
// It is the walkthrough's pre-flight check: the operator enters a
// token, and this says whether it can do the thing the walkthrough is
// about to attempt, before anything is attempted.
//
// The check is a READ against the zone, not a write: a GET on the
// zone's DNS records proves the token can see the zone and read its
// records, and it leaves no trace. It does NOT prove the token can
// create the TXT record that DNS-01 validation needs - only a write
// proves that, and a validation walkthrough must not leave records
// behind.
//
// So the two are reported separately, and the walkthrough says which
// one it is. A token that can read but not write is enough to
// diagnose "this token has the wrong permissions"; the definitive
// check happens on the first real issuance, whose failure names the
// missing permission from Cloudflare itself.
type TokenCheck struct {
	// ZoneID is the zone the check was made against.
	ZoneID string
	// ZoneName is that zone's name.
	ZoneName string
	// CanReadDNS is true when the token could read the zone's DNS
	// records.
	CanReadDNS bool
	// Detail explains what was found, for an operator to read.
	Detail string
}

// VerifyToken checks a token against a zone.
//
// The failure messages name the SPECIFIC thing that is missing, because
// "the token is invalid" sends the operator to Cloudflare's dashboard
// with no idea which permission to add. A token that cannot see the
// zone at all, and a token that can see the zone but not its records,
// are different problems with different fixes.
func VerifyToken(ctx context.Context, token, zoneID string) (TokenCheck, error) {
	if strings.TrimSpace(token) == "" {
		return TokenCheck{}, fmt.Errorf("cloudflare: the API token is empty")
	}
	if strings.TrimSpace(zoneID) == "" {
		return TokenCheck{}, fmt.Errorf("cloudflare: the zone ID is empty")
	}
	check := TokenCheck{ZoneID: zoneID}

	zonePath := fmt.Sprintf("/zones/%s", url.PathEscape(zoneID))
	zoneResp, err := doRequest(ctx, http.MethodGet, zonePath, token, nil)
	if err != nil {
		var apiErr *apiErrorResult
		if errors.As(err, &apiErr) {
			switch apiErr.Code {
			case 10000, 1049:
				return check, fmt.Errorf("cloudflare: this token cannot see zone %s; a zone-scoped token must be scoped to that zone, and the token must carry the Zone:Zone:Read permission", zoneID)
			case 9109:
				return check, fmt.Errorf("cloudflare: this token is not valid; Cloudflare rejected it")
			}
		}
		return check, fmt.Errorf("cloudflare: checking the token against zone %s: %w", zoneID, err)
	}
	var zone struct {
		Name    string `json:"name"`
		Account struct {
			Name string `json:"name"`
		} `json:"account"`
	}
	if err := json.Unmarshal(zoneResp.Result, &zone); err != nil {
		return check, fmt.Errorf("cloudflare: decoding zone %s: %w", zoneID, err)
	}
	check.ZoneName = zone.Name
	check.Detail = fmt.Sprintf("the token can see zone %q (%s) in account %q", zone.Name, zoneID, zone.Account.Name)

	// Reading the zone's DNS records is the read half of the check.
	recordsPath := fmt.Sprintf("/zones/%s/dns_records?per_page=1", url.PathEscape(zoneID))
	if _, err := doRequest(ctx, http.MethodGet, recordsPath, token, nil); err != nil {
		var apiErr *apiErrorResult
		if errors.As(err, &apiErr) {
			switch apiErr.Code {
			case 9109:
				return check, fmt.Errorf("cloudflare: this token is not valid; Cloudflare rejected it")
			case 10000:
				return check, fmt.Errorf("cloudflare: this token cannot read DNS records in zone %s; the token needs the Zone:DNS:Read permission", zoneID)
			}
		}
		return check, fmt.Errorf("cloudflare: this token cannot read DNS records in zone %s: %w", zoneID, err)
	}
	check.CanReadDNS = true
	return check, nil
}
