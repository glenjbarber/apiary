package main

import "testing"

func TestRequireTLSPair_BothSetIsFine(t *testing.T) {
	if err := requireTLSPair("/path/cert.pem", "/path/key.pem"); err != nil {
		t.Errorf("requireTLSPair() error = %v, want nil when both are set", err)
	}
}

func TestRequireTLSPair_OnlyCertIsAnError(t *testing.T) {
	if err := requireTLSPair("/path/cert.pem", ""); err == nil {
		t.Error("requireTLSPair() error = nil, want an error when only tls_cert is set")
	}
}

func TestRequireTLSPair_OnlyKeyIsAnError(t *testing.T) {
	if err := requireTLSPair("", "/path/key.pem"); err == nil {
		t.Error("requireTLSPair() error = nil, want an error when only tls_key is set")
	}
}
