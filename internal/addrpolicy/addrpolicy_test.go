package addrpolicy

import (
	"strings"
	"testing"
)

// TestValidateDialTargetRejectsUnusableHosts covers exactly the
// wildcard family: every one of these is a legal net.Listen address and
// an unusable net.Dial one, which is the whole reason the two
// validators are separate functions.
func TestValidateDialTargetRejectsUnusableHosts(t *testing.T) {
	for _, tc := range []struct {
		name, addr, wantIn string
	}{
		{"IPv4 wildcard", "0.0.0.0:17700", "unspecified"},
		{"IPv6 wildcard, short form", "[::]:17700", "unspecified"},
		{"IPv6 wildcard, on another port", "[::]:8080", "unspecified"},
		{"empty host", ":17700", "no host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDialTarget("manager_addr", tc.addr)
			if err == nil {
				t.Fatalf("ValidateDialTarget(%q) = nil, want a rejection", tc.addr)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("ValidateDialTarget(%q) = %v, want the message to mention %q", tc.addr, err, tc.wantIn)
			}
			if !strings.Contains(err.Error(), "manager_addr") {
				t.Errorf("ValidateDialTarget(%q) = %v, want the message to name the field", tc.addr, err)
			}
			if !strings.Contains(err.Error(), "connection refused") {
				t.Errorf("ValidateDialTarget(%q) = %v, want it to say why: dialing a wildcard is refused", tc.addr, err)
			}
		})
	}
}

// TestValidateDialTargetAcceptsRealDestinations pins the other half of
// the rule. The production colony's working rpc_addr is a DNS
// hostname, and a numeric LAN address must keep parsing here even
// though it cannot verify against Apiary's certificates - refusing it
// would move a TLS problem into a config check and hide it, and this
// function is not entitled to know which certificates exist.
func TestValidateDialTargetAcceptsRealDestinations(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:17700",
		"localhost:17700",
		"brood.lab3.home.arpa:17700",
		"10.90.0.94:17700",
		"[::1]:17700",
		"comb-1.lab3.home.arpa:17700",
	} {
		if err := ValidateDialTarget("manager_addr", addr); err != nil {
			t.Errorf("ValidateDialTarget(%q) = %v, want it accepted", addr, err)
		}
	}
}

// TestValidateDialTargetStillRequiresAPort makes sure the new package
// did not quietly become more permissive than the callers it replaces.
func TestValidateDialTargetStillRequiresAPort(t *testing.T) {
	for _, tc := range []struct{ addr, wantIn string }{
		{"127.0.0.1", "host:port"},
		{"127.0.0.1:", "no port"},
		{"127.0.0.1:0", "invalid port"},
		{"127.0.0.1:70000", "invalid port"},
		{"127.0.0.1:http", "invalid port"},
		{"", "host:port"},
	} {
		err := ValidateDialTarget("manager_addr", tc.addr)
		if err == nil {
			t.Errorf("ValidateDialTarget(%q) = nil, want a rejection mentioning %q", tc.addr, tc.wantIn)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("ValidateDialTarget(%q) = %v, want the message to mention %q", tc.addr, err, tc.wantIn)
		}
	}
}

// TestValidateBindAddressAcceptsWhatAListenerCanUse is the reason this
// is a second function. Every one of these is refused by
// ValidateDialTarget and must be accepted here: they are the addresses
// a daemon legitimately listens on, and rejecting them would break
// every existing deployment and the frontend's own LAN-exposed HTTP
// listener.
func TestValidateBindAddressAcceptsWhatAListenerCanUse(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:17700",
		"[::]:17700",
		":8081", // the empty host is the wildcard, written the other way
		"127.0.0.1:17700",
		"[::1]:17700",
		"localhost:17700",
		"10.90.0.94:17700",
		"brood.lab3.home.arpa:17700",
	} {
		if err := ValidateBindAddress("http_addr", addr); err != nil {
			t.Errorf("ValidateBindAddress(%q) = %v, want it accepted", addr, err)
		}
	}
}

func TestValidateBindAddressStillRequiresAPort(t *testing.T) {
	for _, tc := range []struct{ addr, wantIn string }{
		{"0.0.0.0", "host:port"},
		{"0.0.0.0:", "no port"},
		{"0.0.0.0:notaport", "invalid port"},
		{"nonsense", "host:port"},
	} {
		err := ValidateBindAddress("http_addr", tc.addr)
		if err == nil {
			t.Errorf("ValidateBindAddress(%q) = nil, want a rejection mentioning %q", tc.addr, tc.wantIn)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("ValidateBindAddress(%q) = %v, want the message to mention %q", tc.addr, err, tc.wantIn)
		}
		if !strings.Contains(err.Error(), "http_addr") {
			t.Errorf("ValidateBindAddress(%q) = %v, want the message to name the field", tc.addr, err)
		}
	}
}

// TestFieldNameAppearsInEveryMessage is the small property that makes
// these errors usable: an operator reading a failure from
// restshimdconfig or frontendconfig must be told which line of which
// file to edit, and every failure path here goes through the field name
// rather than hard-coding one.
func TestFieldNameAppearsInEveryMessage(t *testing.T) {
	for _, field := range []string{"manager_addr", "http_addr", "rpc_addr"} {
		for _, addr := range []string{"", "0.0.0.0:17700", ":17700", "127.0.0.1", "127.0.0.1:0"} {
			if err := ValidateDialTarget(field, addr); err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("ValidateDialTarget(%q, %q) = %v, want an error naming the field", field, addr, err)
			}
		}
		// The bind validator accepts the wildcard and the empty host by
		// design, so only the malformed values are left to name a field.
		for _, addr := range []string{"", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:99999"} {
			if err := ValidateBindAddress(field, addr); err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("ValidateBindAddress(%q, %q) = %v, want an error naming the field", field, addr, err)
			}
		}
	}
}
