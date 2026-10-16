package watchgoose

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendWithRetryAcceptsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	if err := sendWithRetry(context.Background(), server.Client(), server.URL); err != nil {
		t.Fatalf("sendWithRetry() error: %v", err)
	}
}

func TestSendWithRetryRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := sendWithRetry(context.Background(), server.Client(), server.URL); err != nil {
		t.Fatalf("sendWithRetry() error: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("request count = %d, want 3", got)
	}
}

func TestSendWithRetryDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "invalid check", http.StatusForbidden)
	}))
	defer server.Close()

	err := sendWithRetry(context.Background(), server.Client(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("sendWithRetry() error = %v, want HTTP 403", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("request count = %d, want 1", got)
	}
}

func TestSendWithRetryStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Timeout: time.Second}

	err := sendWithRetry(ctx, client, "http://127.0.0.1:1")
	if err == nil || err != context.Canceled {
		t.Fatalf("sendWithRetry() error = %v, want context.Canceled", err)
	}
}
