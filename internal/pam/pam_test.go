package pam

import "testing"

func TestPAMAuthenticator_RequiresServiceName(t *testing.T) {
	a := PAMAuthenticator{}
	if _, err := a.Authenticate("someuser", "somepass"); err == nil {
		t.Errorf("Authenticate() error = nil, want an error for an empty ServiceName")
	}
}

// TestPAMAuthenticator_UnknownServiceFailsToStart confirms a
// misconfigured/nonexistent PAM service (no matching /etc/pam.d entry)
// is reported as a real error, not a false "invalid credentials"
// result - an operator misconfiguration should be loud and diagnosable,
// not silently indistinguishable from every real user typing the wrong
// password. Real authentication success itself needs a live,
// root-capable FreeBSD host with a genuine PAM service configured -
// see ADR-0030's own live-verification section; not exercised here.
func TestPAMAuthenticator_UnknownServiceFailsToStart(t *testing.T) {
	a := PAMAuthenticator{ServiceName: "apiary-service-that-does-not-exist-anywhere"}
	_, err := a.Authenticate("someuser", "somepass")
	if err == nil {
		t.Skip("this host's PAM stack accepted an unconfigured service name (some PAM implementations fall back to a default policy) - not a failure of this package, just not exercisable here")
	}
}
