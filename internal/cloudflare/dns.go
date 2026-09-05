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
)

// apiBaseURL is Cloudflare's real API base - overridable in tests via
// baseURL below, mirroring the established "swap the real backend for
// an httptest.Server" pattern this project already uses elsewhere.
const apiBaseURL = "https://api.cloudflare.com/client/v4"

var baseURL = apiBaseURL

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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling cloudflare API: %w", err)
	}
	defer resp.Body.Close()

	var parsed apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
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

// DeleteCNAME idempotently removes hostname's CNAME record. A record
// that's already gone (never created, or removed out-of-band in
// Cloudflare's own dashboard - a disclosed, accepted v1 gap, see
// ADR-0063) is treated as success, never an error.
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
