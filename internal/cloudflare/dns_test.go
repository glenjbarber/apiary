package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withFakeAPI points baseURL at a local httptest.Server for the
// duration of one test, restoring the real Cloudflare API URL after.
func withFakeAPI(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		baseURL = old
	})
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func TestEnsureCNAME_CreatesWhenNoRecordExists(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "dns_records") && r.Method == http.MethodGet {
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[]`)})
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{}`)})
	})

	if err := EnsureCNAME(context.Background(), "tok", "zone-1", "web.example.com", "tunnel-id.cfargotunnel.com"); err != nil {
		t.Fatalf("EnsureCNAME() error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST (create)", gotMethod)
	}
	if !strings.Contains(gotPath, "/zones/zone-1/dns_records") {
		t.Errorf("path = %q, want /zones/zone-1/dns_records", gotPath)
	}
	if gotBody["name"] != "web.example.com" || gotBody["content"] != "tunnel-id.cfargotunnel.com" {
		t.Errorf("body = %+v, want name/content set", gotBody)
	}
}

func TestEnsureCNAME_UpdatesWhenTargetDiffers(t *testing.T) {
	var gotMethod, gotPath string
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[{"id":"rec-1","type":"CNAME","name":"web.example.com","content":"old-tunnel.cfargotunnel.com","proxied":true}]`)})
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.Path
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{}`)})
	})

	if err := EnsureCNAME(context.Background(), "tok", "zone-1", "web.example.com", "new-tunnel.cfargotunnel.com"); err != nil {
		t.Fatalf("EnsureCNAME() error: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT (update)", gotMethod)
	}
	if !strings.Contains(gotPath, "dns_records/rec-1") {
		t.Errorf("path = %q, want the existing record's own id", gotPath)
	}
}

func TestEnsureCNAME_NoOpWhenAlreadyCorrect(t *testing.T) {
	writeCalls := 0
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[{"id":"rec-1","type":"CNAME","name":"web.example.com","content":"tunnel-id.cfargotunnel.com","proxied":true}]`)})
			return
		}
		writeCalls++
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{}`)})
	})

	if err := EnsureCNAME(context.Background(), "tok", "zone-1", "web.example.com", "tunnel-id.cfargotunnel.com"); err != nil {
		t.Fatalf("EnsureCNAME() error: %v", err)
	}
	if writeCalls != 0 {
		t.Errorf("expected zero create/update calls when the record already matches, got %d", writeCalls)
	}
}

func TestDeleteCNAME_NotFoundIsSuccess(t *testing.T) {
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[]`)})
			return
		}
		t.Fatalf("unexpected %s call for a record that doesn't exist", r.Method)
	})

	if err := DeleteCNAME(context.Background(), "tok", "zone-1", "web.example.com"); err != nil {
		t.Fatalf("DeleteCNAME() error: %v, want nil (idempotent, mirrors pf.Manager.Flush's own not-found-is-success fix)", err)
	}
}

func TestDeleteCNAME_DeletesExistingRecord(t *testing.T) {
	var gotMethod, gotPath string
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[{"id":"rec-1","type":"CNAME","name":"web.example.com","content":"tunnel-id.cfargotunnel.com"}]`)})
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.Path
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{}`)})
	})

	if err := DeleteCNAME(context.Background(), "tok", "zone-1", "web.example.com"); err != nil {
		t.Fatalf("DeleteCNAME() error: %v", err)
	}
	if gotMethod != http.MethodDelete || !strings.Contains(gotPath, "dns_records/rec-1") {
		t.Errorf("method/path = %s/%s, want DELETE on the existing record's own id", gotMethod, gotPath)
	}
}

func TestDeleteCNAME_APINotFoundErrorIsSuccess(t *testing.T) {
	// A record can disappear between the list and the delete call (a
	// real race, or an out-of-band dashboard deletion) - Cloudflare's
	// own "record not found" error code on the DELETE itself must also
	// be treated as success, not just an empty list.
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[{"id":"rec-1","type":"CNAME","name":"web.example.com","content":"x"}]`)})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, apiResponse{Success: false, Errors: []apiError{{Code: notFoundErrorCode, Message: "Record does not exist."}}})
	})

	if err := DeleteCNAME(context.Background(), "tok", "zone-1", "web.example.com"); err != nil {
		t.Fatalf("DeleteCNAME() error: %v, want nil for Cloudflare's own not-found error code", err)
	}
}

func TestEnsureCNAME_SurfacesRealAPIErrors(t *testing.T) {
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[]`)})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, apiResponse{Success: false, Errors: []apiError{{Code: 9109, Message: "Invalid access token"}}})
	})

	err := EnsureCNAME(context.Background(), "bad-token", "zone-1", "web.example.com", "tunnel-id.cfargotunnel.com")
	if err == nil || !strings.Contains(err.Error(), "Invalid access token") {
		t.Fatalf("EnsureCNAME() error = %v, want the real Cloudflare error surfaced, not swallowed", err)
	}
}

// The walkthrough's pre-flight: resolve a zone name, and check what a
// token may do against it.
//
// These tests are about the REFUSALS being specific, because a token
// that is mis-scoped is the most common way this flow goes wrong, and
// "the token is invalid" sends the operator to Cloudflare's dashboard
// with no idea which permission to add.

func TestListZones_ReturnsTheZonesTheTokenCanSee(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zones" {
			t.Errorf("path = %q, want /zones", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[{"id":"z2","name":"beta.example.com"},{"id":"z1","name":"alpha.example.com"}]`)})
	}))
	defer server.Close()
	reset := SetBaseURLForTest(server.URL)
	defer reset()

	zones, err := ListZones(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if len(zones) != 2 {
		t.Fatalf("ListZones returned %d zones, want 2", len(zones))
	}
	// Sorted by name, because an operator has to choose from this list.
	if zones[0].Name != "alpha.example.com" || zones[1].Name != "beta.example.com" {
		t.Errorf("zones are not sorted by name: %+v", zones)
	}
}

func TestFindZoneByName_RefusesAnAmbiguousName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[{"id":"z1","name":"example.com"},{"id":"z2","name":"example.com"}]`)})
	}))
	defer server.Close()
	reset := SetBaseURLForTest(server.URL)
	defer reset()

	_, err := FindZoneByName(context.Background(), "tok", "example.com")
	if err == nil {
		t.Fatal("an ambiguous zone name was resolved to one of its candidates")
	}
	if !strings.Contains(err.Error(), "more than one account") {
		t.Errorf("the refusal %q does not explain that the name is ambiguous", err)
	}
	// Both candidates are named, so the operator can disambiguate.
	if !strings.Contains(err.Error(), "z1") || !strings.Contains(err.Error(), "z2") {
		t.Errorf("the refusal %q does not name both candidate zone IDs", err)
	}
}

func TestFindZoneByName_RefusesAnUnknownZone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[]`)})
	}))
	defer server.Close()
	reset := SetBaseURLForTest(server.URL)
	defer reset()

	_, err := FindZoneByName(context.Background(), "tok", "nope.example.com")
	if err == nil {
		t.Fatal("an unknown zone name was resolved")
	}
	if !strings.Contains(err.Error(), "no zone named") {
		t.Errorf("the refusal %q does not say the zone is not visible", err)
	}
}

func TestVerifyToken_SaysWhichPermissionIsMissing(t *testing.T) {
	// A token that cannot see the zone at all, and one that can see
	// the zone but not its records, are different problems with
	// different fixes.
	cases := []struct {
		name       string
		zoneStatus int
		zoneBody   string
		want       string
	}{
		{
			name:       "cannot see the zone",
			zoneStatus: 200,
			zoneBody:   `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`,
			want:       "Zone:Zone:Read",
		},
		{
			name:       "the token is not valid",
			zoneStatus: 200,
			zoneBody:   `{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`,
			want:       "not valid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.zoneStatus)
				_, _ = w.Write([]byte(tc.zoneBody))
			}))
			defer server.Close()
			reset := SetBaseURLForTest(server.URL)
			defer reset()

			_, err := VerifyToken(context.Background(), "tok", "zone-1")
			if err == nil {
				t.Fatal("VerifyToken accepted a token that cannot use the zone")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal %q does not name %q, so the operator is not told which permission to add", err, tc.want)
			}
		})
	}
}

func TestVerifyToken_ReportsAUsableToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones/zone-1":
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{"name":"example.com","account":{"name":"acme"}}`)})
		case strings.HasPrefix(r.URL.Path, "/zones/zone-1/dns_records"):
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[]`)})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	reset := SetBaseURLForTest(server.URL)
	defer reset()

	// A distinctive token, so the leak check below is meaningful: a
	// short token like "tok" is a substring of the word "token" and
	// would trip it spuriously.
	const secret = "s3cr3t-cf-token-value"
	check, err := VerifyToken(context.Background(), secret, "zone-1")
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if !check.CanReadDNS {
		t.Error("CanReadDNS = false, want true")
	}
	if check.ZoneName != "example.com" {
		t.Errorf("ZoneName = %q, want example.com", check.ZoneName)
	}
	// The detail names the zone and the account, and never the token.
	if !strings.Contains(check.Detail, "example.com") {
		t.Errorf("the detail %q does not name the zone", check.Detail)
	}
	if strings.Contains(check.Detail, secret) {
		t.Errorf("the detail %q leaks the token", check.Detail)
	}
}
