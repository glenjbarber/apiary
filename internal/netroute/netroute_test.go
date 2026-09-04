package netroute

import (
	"errors"
	"testing"
)

func TestParseDefaultRouteOutput_NormalOutput(t *testing.T) {
	// A representative `route -n get default` shape - see the package
	// doc comment: exact field layout is a best-effort guess pending
	// real-hardware confirmation.
	out := `   route to: default
destination: default
       mask: default
    gateway: 10.50.0.1
  interface: em0
      flags: <UP,GATEWAY,DONE,STATIC>
`
	iface, hasRoute, err := parseDefaultRouteOutput(out, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasRoute || iface != "em0" {
		t.Errorf("got iface=%q hasRoute=%v, want em0/true", iface, hasRoute)
	}
}

func TestParseDefaultRouteOutput_NoDefaultRoute(t *testing.T) {
	// Flagged pending confirmation against real FreeBSD output - see
	// package doc comment.
	out := "route: writing to routing socket: not in table\n"
	iface, hasRoute, err := parseDefaultRouteOutput(out, errors.New("exit status 1"))
	if err != nil {
		t.Fatalf("a definitive no-route signature must map to (StatusFalse-equivalent), not an error: %v", err)
	}
	if hasRoute || iface != "" {
		t.Errorf("got iface=%q hasRoute=%v, want \"\"/false", iface, hasRoute)
	}
}

func TestParseDefaultRouteOutput_ExecFailure(t *testing.T) {
	_, hasRoute, err := parseDefaultRouteOutput("", errors.New("exec: \"route\": executable file not found in $PATH"))
	if err == nil {
		t.Fatal("an exec failure must surface as an error (-> StatusUnknown), never a definitive answer")
	}
	if hasRoute {
		t.Error("hasRoute = true on exec failure")
	}
}

func TestParseDefaultRouteOutput_UnrecognizedOutput(t *testing.T) {
	_, hasRoute, err := parseDefaultRouteOutput("some completely unexpected garbage\n", nil)
	if err == nil {
		t.Fatal("unrecognized output must surface as an error (-> StatusUnknown), not a guessed answer")
	}
	if hasRoute {
		t.Error("hasRoute = true on unrecognized output")
	}
}
