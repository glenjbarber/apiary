package frontend

import (
	"bytes"
	"fmt"
	"os/exec"

	"github.com/glenjbarber/apiary/internal/manager"
)

// minPasswordLength is a fixed floor, not a configurable policy - this
// project has no precedent for tunable password rules, and a single
// hard-coded minimum is enough for what's actually being protected here
// (a handful of local test/demo accounts on a personal cluster).
const minPasswordLength = 8

// PasswordSetter is the subset of real system behavior
// handleChangePassword needs, defined locally so tests can supply a
// fake without touching real UNIX accounts - the same reasoning
// isoManager/VNCLookup/pam.Authenticator already follow elsewhere in
// this project.
type PasswordSetter interface {
	SetPassword(username, newPassword string) error
}

// UnixPasswordSetter implements PasswordSetter via pw(8) - the same
// `pw usermod <user> -h 0` invocation (new password piped to stdin)
// already used by hand to set up this project's own admin/operator/
// viewer test accounts. Requires root, which cmd/frontend already runs
// as (confirmed live - see ADR-0039).
type UnixPasswordSetter struct{}

func (UnixPasswordSetter) SetPassword(username, newPassword string) error {
	cmd := exec.Command("pw", "usermod", username, "-h", "0")
	cmd.Stdin = bytes.NewBufferString(newPassword + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pw usermod %s: %w: %s", username, err, stderr.String())
	}
	return nil
}

// canChangePassword implements the authorization rule confirmed
// directly for this feature (ADR-0039): Admin can change its own,
// Operator's, and Viewer's password; Operator can change its own and
// Viewer's, never Admin's; Viewer can change no one's, not even its
// own. This isn't a plain "at or below my own rank" self-inclusive
// rule - Viewer is explicitly excluded from touching even itself - so
// it's expressed as an explicit veto plus the existing rank comparison
// rather than reusing requireRole's single-threshold shape directly.
func canChangePassword(actorRole, targetRole manager.Role) bool {
	return actorRole != manager.RoleViewer && actorRole.Satisfies(targetRole)
}
