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
