// Package hostconfig exports a redacted, read-only snapshot of a
// Hive's host-level configuration - the gap raftd's own -export
// (ADR-0051) deliberately doesn't cover, since these are plain host
// files, not raft-replicated ephemeral state: /etc/rc.conf (this
// node's actual daemon flags, including a live -peer-api-key secret
// this package redacts - see ADR-0067's own finding 8), /etc/pf.conf
// (the host's own packet-filter rules, distinct from Apiary's per-VM
// firewall records already replicated through raft), and
// /etc/master.passwd (PAM/local-account data, redacted to drop every
// live password hash).
//
// This package is deliberately export-only. There is no restore/apply
// path: automatically overwriting a live host's own login database or
// firewall rules is a materially different, higher-risk problem than
// reading them (a bug here could lock out every account or break
// network connectivity), and needs its own careful design and live
// testing on non-critical infrastructure before it's built - explicitly
// deferred, not an oversight. Nothing in this package is ever reachable
// over the network; it's a local, operator-run one-shot CLI action
// (cmd/managerd's own -export-host-config), the same posture cmd/raftd's
// -export/-restore already established for raft state.
package hostconfig

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Files names the three host config source paths an export reads.
// Zero values fall back to this project's own documented locations
// (see withDefaults).
type Files struct {
	RcConfPath       string
	PfConfPath       string
	MasterPasswdPath string
}

func (f Files) withDefaults() Files {
	if f.RcConfPath == "" {
		f.RcConfPath = "/etc/rc.conf"
	}
	if f.PfConfPath == "" {
		f.PfConfPath = "/etc/pf.conf"
	}
	if f.MasterPasswdPath == "" {
		f.MasterPasswdPath = "/etc/master.passwd"
	}
	return f
}

// RedactMasterPasswd rewrites master.passwd's own colon-delimited
// format (see passwd(5)), replacing every account line's encrypted
// password field (field 2) with a literal "*" - FreeBSD's own locked/
// no-password convention, never an empty field, which would
// dangerously mean "no password required." Every other field
// (username, uid, gid, class, change/expire times, gecos, home, shell)
// is preserved verbatim, so account structure stays inspectable
// without ever persisting a live, crackable hash anywhere this export
// might end up. Blank lines, comments (a leading '#'), and any line
// that doesn't look like a real account record pass through unchanged
// rather than being guessed at.
func RedactMasterPasswd(r io.Reader) ([]byte, error) {
	var out bytes.Buffer
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 2 {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		fields[1] = "*"
		out.WriteString(strings.Join(fields, ":"))
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("hostconfig: reading master.passwd: %w", err)
	}
	return out.Bytes(), nil
}

// peerAPIKeyPattern matches "-peer-api-key <value>" inside rc.conf's
// own quoted args string (e.g. apiary_managerd_args="... -peer-api-key
// apk_XXXX ..."), the one live secret confirmed to appear in this
// project's own rc.conf (ADR-0067's finding 8: it was found
// world-readable there). [^\s"]+ (not \S+) stops at a closing quote
// too, so a flag placed last inside the quoted string is still
// redacted correctly, not left with a dangling '"' consumed into the
// match.
var peerAPIKeyPattern = regexp.MustCompile(`(-peer-api-key )([^\s"]+)`)

// RedactRcConf redacts every known-sensitive flag value from rc.conf's
// content. This is a small, explicit, necessarily-incomplete allowlist
// (currently just -peer-api-key) - not a general secret scanner - so
// any future flag that carries a live secret needs to be added here
// deliberately.
func RedactRcConf(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("hostconfig: reading rc.conf: %w", err)
	}
	return peerAPIKeyPattern.ReplaceAll(body, []byte("${1}REDACTED")), nil
}

// Export reads files' rc.conf/pf.conf/master.passwd, redacts the two
// that can carry a live secret, and writes all three into outDir as
// plain, separately readable files (rc.conf.redacted, pf.conf,
// master.passwd.redacted) - deliberately not a single opaque archive
// format, since this is a human-facing backup an operator should be
// able to open and read directly, not a machine-restore format (see
// the package doc comment for why there is no restore path at all).
// outDir and every file written into it are created 0700/0600 -
// redacted or not, this is still account and daemon-configuration data
// worth keeping off of a shared/world-readable path.
func Export(files Files, outDir string) error {
	files = files.withDefaults()
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("hostconfig: creating output dir %q: %w", outDir, err)
	}
	if err := exportRedacted(files.RcConfPath, filepath.Join(outDir, "rc.conf.redacted"), RedactRcConf); err != nil {
		return err
	}
	if err := exportRedacted(files.MasterPasswdPath, filepath.Join(outDir, "master.passwd.redacted"), RedactMasterPasswd); err != nil {
		return err
	}
	if err := copyFile(files.PfConfPath, filepath.Join(outDir, "pf.conf")); err != nil {
		return err
	}
	return nil
}

func exportRedacted(srcPath, dstPath string, redact func(io.Reader) ([]byte, error)) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("hostconfig: opening %q: %w", srcPath, err)
	}
	defer f.Close()
	redacted, err := redact(f)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dstPath, redacted, 0o600); err != nil {
		return fmt.Errorf("hostconfig: writing %q: %w", dstPath, err)
	}
	return nil
}

func copyFile(srcPath, dstPath string) error {
	body, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("hostconfig: reading %q: %w", srcPath, err)
	}
	if err := os.WriteFile(dstPath, body, 0o600); err != nil {
		return fmt.Errorf("hostconfig: writing %q: %w", dstPath, err)
	}
	return nil
}
