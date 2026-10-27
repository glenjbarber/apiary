package manager

import (
	"context"
	"strings"
	"testing"
)

// TestPeerReporter_RefusesToOpenAConsoleTunnelOverPlaintext is the
// second hop of the same property.
//
// A VM console tunnel carries a RAW RFB STREAM: the guest's screen and
// its keyboard, live. It is not a status read and not a summary - it
// is interactive control of the guest.
//
// The existing guards refuse a peer API key and a restart-guardrail
// token from crossing the network in cleartext. This one was missed,
// because the console tunnel attaches no credential and so never went
// through the check that would have caught it.
//
// The address is one nothing is listening on, so the only way this
// could succeed is the dial having gone out anyway.
func TestPeerReporter_RefusesToOpenAConsoleTunnelOverPlaintext(t *testing.T) {
	p := NewPeerReporter("", false, nil)

	_, err := p.OpenVMConsole(context.Background(), "127.0.0.1:1", "vm-1")
	if err == nil {
		t.Fatal("OpenVMConsole() with peer_tls=false = nil error, want a refusal to carry the console stream in cleartext")
	}
	if !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("the refusal does not say the console would cross the network in cleartext: %v", err)
	}
	if !strings.Contains(err.Error(), "peer_tls") {
		t.Errorf("the refusal does not name the setting that has to change: %v", err)
	}
}

// TestPeerReporter_RefusesToOpenAConsoleTunnelWithAnAPIKeyOverPlaintext
// covers the case where a node has BOTH a peer API key and peer TLS
// off. The API key path is already refused by the existing guard, and
// the console must be refused by its own - the console carries the
// guest's screen whether or not a credential is attached to it.
func TestPeerReporter_RefusesToOpenAConsoleTunnelWithAnAPIKeyOverPlaintext(t *testing.T) {
	p := NewPeerReporter("test-key", false, nil)

	_, err := p.OpenVMConsole(context.Background(), "127.0.0.1:1", "vm-1")
	if err == nil {
		t.Fatal("OpenVMConsole() with peer_tls=false and an API key set = nil error, want a refusal")
	}
}
