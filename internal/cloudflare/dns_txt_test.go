package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCreateTXT_PostsRecordAndReturnsID(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{"id":"rec-123","type":"TXT","name":"_acme-challenge.example.com","content":"challenge-value"}`)})
	})

	id, err := CreateTXT(context.Background(), "tok", "zone-1", "_acme-challenge.example.com", "challenge-value")
	if err != nil {
		t.Fatalf("CreateTXT() error: %v", err)
	}
	if id != "rec-123" {
		t.Errorf("id = %q, want rec-123", id)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.Contains(gotPath, "/zones/zone-1/dns_records") {
		t.Errorf("path = %q, want /zones/zone-1/dns_records", gotPath)
	}
	if gotBody["type"] != "TXT" || gotBody["name"] != "_acme-challenge.example.com" || gotBody["content"] != "challenge-value" {
		t.Errorf("body = %+v, want TXT record for the challenge name/value", gotBody)
	}
}

func TestCreateTXT_SurfacesAPIError(t *testing.T) {
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, apiResponse{Success: false, Errors: []apiError{{Code: 1003, Message: "invalid zone"}}})
	})

	if _, err := CreateTXT(context.Background(), "tok", "zone-1", "_acme-challenge.example.com", "v"); err == nil {
		t.Fatal("CreateTXT() error = nil, want non-nil on API failure")
	}
}

func TestDeleteTXT_DeletesByRecordID(t *testing.T) {
	var gotMethod, gotPath string
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{}`)})
	})

	if err := DeleteTXT(context.Background(), "tok", "zone-1", "rec-123"); err != nil {
		t.Fatalf("DeleteTXT() error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if !strings.Contains(gotPath, "/zones/zone-1/dns_records/rec-123") {
		t.Errorf("path = %q, want .../dns_records/rec-123", gotPath)
	}
}

func TestDeleteTXT_EmptyRecordIDIsNoop(t *testing.T) {
	called := false
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{}`)})
	})

	if err := DeleteTXT(context.Background(), "tok", "zone-1", ""); err != nil {
		t.Fatalf("DeleteTXT() error: %v", err)
	}
	if called {
		t.Error("DeleteTXT with empty recordID made an API call, want no-op")
	}
}

func TestDeleteTXT_NotFoundIsSuccess(t *testing.T) {
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, apiResponse{Success: false, Errors: []apiError{{Code: notFoundErrorCode, Message: "record does not exist"}}})
	})

	if err := DeleteTXT(context.Background(), "tok", "zone-1", "rec-gone"); err != nil {
		t.Errorf("DeleteTXT() error = %v, want nil for an already-gone record", err)
	}
}
