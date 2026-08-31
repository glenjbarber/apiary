// Package pam authenticates a username/password pair against the
// host's real PAM stack (ADR-0030), letting cmd/frontend's web UI
// login check real system identity instead of a single shared
// credential. This is the one, deliberately narrow integration point:
// Apiary itself never speaks Kerberos or LDAP - whatever PAM service
// this package is pointed at (configured by the operator in
// /etc/pam.d, entirely outside this code) decides whether that means
// local UNIX accounts (pam_unix), Kerberos (pam_krb5 - FreeBSD's own
// base krb5 is Heimdal, not MIT, but its pam_krb5 module plugs into
// PAM the same way), or Active Directory (pam_ldap/pam_winbind).
//
// This package requires cgo (it wraps the system libpam via
// github.com/msteinert/pam/v2) - cmd/frontend must be built with
// CGO_ENABLED=1 on a real FreeBSD host (or a proper cross toolchain),
// unlike every other Apiary binary, which cross-compiles cleanly from
// any platform. See ADR-0030's own build-consequences section.
package pam

import (
	"fmt"

	"github.com/msteinert/pam/v2"
)

// Authenticator abstracts how the web UI verifies a username/password
// pair against a real identity backend - defined so internal/frontend
// can be tested against a fake, the same isoManager/VNCLookup/
// VLANStatus pattern this project already follows everywhere else.
// Authenticate reports only whether the credentials are valid; it
// never distinguishes "wrong password" from "unknown user" (PAM
// itself deliberately makes this indistinguishable, to avoid leaking
// which usernames exist).
type Authenticator interface {
	Authenticate(username, password string) (bool, error)
}

// PAMAuthenticator implements Authenticator via the host's real PAM
// stack, under a single configured PAM service name.
type PAMAuthenticator struct {
	// ServiceName is the PAM service to authenticate against - the
	// operator must create a matching /etc/pam.d/<ServiceName> on the
	// host (a one-time prerequisite, the same posture as ADR-0022's
	// pf/dnsmasq setup or ADR-0026's hastd_enable). There is no
	// built-in default: an empty ServiceName is a configuration error,
	// not a fallback to some assumed name.
	ServiceName string
}

// Authenticate runs a real PAM authentication + account-validity check
// (Authenticate then AcctMgmt, so an account PAM itself considers
// expired/locked/disabled is rejected even with a correct password -
// the same two-step sequence login(1)/sshd(8) perform) against the
// configured ServiceName. A failure at either step is reported as
// (false, nil) - an ordinary, expected "wrong credentials" outcome,
// not a Go error; err is reserved for a real inability to even start a
// PAM transaction (e.g. no such PAM service configured on the host at
// all).
func (a PAMAuthenticator) Authenticate(username, password string) (bool, error) {
	if a.ServiceName == "" {
		return false, fmt.Errorf("pam: ServiceName must be set")
	}

	tx, err := pam.StartFunc(a.ServiceName, username, func(style pam.Style, msg string) (string, error) {
		switch style {
		case pam.PromptEchoOff, pam.PromptEchoOn:
			return password, nil
		default:
			// TextInfo/ErrorMsg/BinaryPrompt: nothing to answer with -
			// returning empty rather than erroring lets an
			// informational-only exchange proceed instead of aborting
			// the whole transaction over a message this package has no
			// UI to display anyway.
			return "", nil
		}
	})
	if err != nil {
		return false, fmt.Errorf("pam: starting transaction for service %q: %w", a.ServiceName, err)
	}
	defer tx.End()

	if err := tx.Authenticate(0); err != nil {
		return false, nil
	}
	if err := tx.AcctMgmt(0); err != nil {
		return false, nil
	}
	return true, nil
}
