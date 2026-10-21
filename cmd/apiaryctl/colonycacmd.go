// apiaryctl colony-ca is step 2 of issue #97 (ADR-0155). It is the
// operator-run way to create the Colony CA on a Comb and to issue that
// Comb's raft certificate from it.
//
// SCOPE, deliberately narrow. Neither subcommand changes a running Comb:
// neither restarts a daemon, edits a config file, or flips
// raft_tls_enabled or peer_tls. They write files and print what to
// point raftd.json at. The flip is a later, coordinated, operator-run
// step.
//
//   - init creates the Colony CA with colonyca.GenerateCA. It refuses if
//     a CA already exists, because replacing the CA silently invalidates
//     every leaf the Colony holds.
//   - issue-raft reads the CA and signs one leaf for this Comb with
//     colonyca.IssueLeaf. It refuses to overwrite raft.pem or raft.key.
//
// Both are root-only, because the CA key they handle is the Colony's
// root of trust.
package main

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/colonyca"
)

const (
	// defaultColonyCADir holds ca.pem and ca.key. It is the directory
	// the operator copies the CA certificate out of for other Combs.
	defaultColonyCADir = "/usr/local/etc/apiary/ca"

	// defaultRaftCertDir is where raft.pem and raft.key land, next to
	// the other config files, so raftd.json can name them directly.
	defaultRaftCertDir = "/usr/local/etc/apiary"

	raftCertFile = "raft.pem"
	raftKeyFile  = "raft.key"
)

const colonyCAUsage = `usage: apiaryctl colony-ca init [--dir DIR]
       apiaryctl colony-ca issue-raft [--ca-dir DIR] [--out-dir DIR] --name HOST [--name HOST...] [--ip IP...]

init creates the Colony CA in DIR (default ` + defaultColonyCADir + `):
ca.pem (0644) and ca.key (0600). Run it once, on the first Comb of a new
Colony. It refuses if either file already exists and changes neither.

issue-raft signs this Comb's raft certificate with the CA in --ca-dir
(default ` + defaultColonyCADir + `) and writes raft.pem (0644) and raft.key (0600)
to --out-dir (default ` + defaultRaftCertDir + `). Give every name and address
that peers will dial this Comb by. It refuses if raft.pem or raft.key
already exists.

Both must run as root. Neither restarts anything or edits any config.
`

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runColonyCA(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, colonyCAUsage)
		return 2
	}
	switch args[0] {
	case "init":
		return runColonyCAInit(args[1:])
	case "issue-raft":
		return runColonyCAIssueRaft(args[1:])
	case "help", "-h", "--help":
		fmt.Print(colonyCAUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "apiaryctl colony-ca: unknown subcommand %q\n\n%s", args[0], colonyCAUsage)
		return 2
	}
}

func runColonyCAInit(args []string) int {
	fs := flag.NewFlagSet("apiaryctl colony-ca init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", defaultColonyCADir, "directory to create the CA in")
	fs.Usage = func() { fmt.Fprint(os.Stderr, colonyCAUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl colony-ca init: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl colony-ca init: must run as root.\n\nIt writes the Colony CA key, root-only, to %s. Run it from a root shell, or with sudo.\n", *dir)
		return 1
	}
	return colonyCAInit(*dir, os.Stdout, os.Stderr)
}

// colonyCAInit is runColonyCAInit minus flags and the root check, so
// it can be tested against a temp directory.
func colonyCAInit(dir string, out, errOut *os.File) int {
	certPath := filepath.Join(dir, colonyca.CACertFile)
	keyPath := filepath.Join(dir, colonyca.CAKeyFile)
	if existing := existingPaths(certPath, keyPath); len(existing) > 0 {
		fmt.Fprintf(errOut, "apiaryctl colony-ca init: a Colony CA already exists here (%s).\n", strings.Join(existing, ", "))
		fmt.Fprintln(errOut, "Refusing to create another. Replacing the CA would invalidate every leaf the Colony holds. No file was changed.")
		return 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(errOut, "apiaryctl colony-ca init: creating %s: %v\n", dir, err)
		return 1
	}
	fingerprint, err := colonyca.GenerateCA(dir, time.Now())
	if err != nil {
		fmt.Fprintf(errOut, "apiaryctl colony-ca init: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "apiaryctl colony-ca init: created the Colony CA.\n")
	fmt.Fprintf(out, "  CA certificate: %s (0644)\n", certPath)
	fmt.Fprintf(out, "  CA key:         %s (0600, root only)\n", keyPath)
	fmt.Fprintf(out, "  CA fingerprint (SHA-256): %s\n\n", fingerprint)
	fmt.Fprintln(out, "The CA key is the Colony's root of trust. Keep it on this Comb. Do not copy it anywhere.")
	fmt.Fprintln(out, "Next: apiaryctl colony-ca issue-raft --name HOST [--ip IP] on this Comb.")
	return 0
}

func runColonyCAIssueRaft(args []string) int {
	fs := flag.NewFlagSet("apiaryctl colony-ca issue-raft", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	caDir := fs.String("ca-dir", defaultColonyCADir, "directory holding ca.pem and ca.key")
	outDir := fs.String("out-dir", defaultRaftCertDir, "directory to write raft.pem and raft.key into")
	var names, ips stringList
	fs.Var(&names, "name", "DNS name peers dial this Comb by (repeatable)")
	fs.Var(&ips, "ip", "IP address peers dial this Comb by (repeatable)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, colonyCAUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl colony-ca issue-raft: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if len(names) == 0 && len(ips) == 0 {
		fmt.Fprintln(os.Stderr, "apiaryctl colony-ca issue-raft: give at least one --name or --ip for this Comb.")
		return 2
	}
	parsedIPs := make([]net.IP, 0, len(ips))
	for _, s := range ips {
		ip := net.ParseIP(strings.TrimSpace(s))
		if ip == nil {
			fmt.Fprintf(os.Stderr, "apiaryctl colony-ca issue-raft: %q is not an IP address.\n", s)
			return 2
		}
		parsedIPs = append(parsedIPs, ip)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl colony-ca issue-raft: must run as root.\n\nIt reads the Colony CA key and writes %s and %s. Run it from a root shell, or with sudo.\n",
			filepath.Join(*outDir, raftCertFile), filepath.Join(*outDir, raftKeyFile))
		return 1
	}
	return colonyCAIssueRaft(*caDir, *outDir, names, parsedIPs, time.Now(), os.Stdout, os.Stderr)
}

// colonyCAIssueRaft is runColonyCAIssueRaft minus flags and the root
// check, so it can be tested against temp directories.
func colonyCAIssueRaft(caDir, outDir string, names []string, ips []net.IP, now time.Time, out, errOut *os.File) int {
	caCertPath := filepath.Join(caDir, colonyca.CACertFile)
	caKeyPath := filepath.Join(caDir, colonyca.CAKeyFile)
	certPath := filepath.Join(outDir, raftCertFile)
	keyPath := filepath.Join(outDir, raftKeyFile)

	// Everything is checked before anything is written.
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		fmt.Fprintf(errOut, "apiaryctl colony-ca issue-raft: reading the Colony CA certificate: %v\n", err)
		fmt.Fprintln(errOut, "Run apiaryctl colony-ca init on the Colony's first Comb first, or pass --ca-dir.")
		return 1
	}
	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		fmt.Fprintf(errOut, "apiaryctl colony-ca issue-raft: reading the Colony CA key: %v\n", err)
		return 1
	}
	if existing := existingPaths(certPath, keyPath); len(existing) > 0 {
		fmt.Fprintf(errOut, "apiaryctl colony-ca issue-raft: already exists (%s). Refusing to overwrite. No file was changed.\n", strings.Join(existing, ", "))
		return 1
	}

	certPEM, keyPEM, err := colonyca.IssueLeaf(caCertPEM, caKeyPEM, colonyca.LeafRequest{
		DNSNames: names,
		IPs:      ips,
	}, now)
	if err != nil {
		fmt.Fprintf(errOut, "apiaryctl colony-ca issue-raft: %v\n", err)
		return 1
	}
	fingerprint, err := colonyca.Fingerprint(certPEM)
	if err != nil {
		fmt.Fprintf(errOut, "apiaryctl colony-ca issue-raft: %v\n", err)
		return 1
	}
	leaf, err := parseFirstCert(certPEM)
	if err != nil {
		fmt.Fprintf(errOut, "apiaryctl colony-ca issue-raft: %v\n", err)
		return 1
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(errOut, "apiaryctl colony-ca issue-raft: creating %s: %v\n", outDir, err)
		return 1
	}
	// The key is written first, so a certificate never exists without
	// its key. If the certificate cannot be written, the key this call
	// created is removed again.
	if err := writeNewFile(keyPath, keyPEM, 0o600); err != nil {
		fmt.Fprintf(errOut, "apiaryctl colony-ca issue-raft: %v\n", err)
		return 1
	}
	if err := writeNewFile(certPath, certPEM, 0o644); err != nil {
		_ = os.Remove(keyPath)
		fmt.Fprintf(errOut, "apiaryctl colony-ca issue-raft: %v\n", err)
		return 1
	}

	fmt.Fprintf(out, "apiaryctl colony-ca issue-raft: issued this Comb's raft certificate.\n")
	fmt.Fprintf(out, "  certificate:  %s (0644)\n", certPath)
	fmt.Fprintf(out, "  key:          %s (0600, root only)\n", keyPath)
	fmt.Fprintf(out, "  fingerprint (SHA-256): %s\n", fingerprint)
	fmt.Fprintf(out, "  not after:    %s\n\n", leaf.NotAfter.UTC().Format(time.RFC3339))
	fmt.Fprintln(out, "Set these raftd.json fields when you are ready. Setting them does not enable TLS by itself:")
	fmt.Fprintf(out, "  \"raft_tls_cert\": %q\n", certPath)
	fmt.Fprintf(out, "  \"raft_tls_key\":  %q\n", keyPath)
	fmt.Fprintf(out, "  \"raft_tls_ca\":   %q\n", caCertPath)
	fmt.Fprintln(out, "\nNothing was restarted or reconfigured by this command.")
	return 0
}

// existingPaths returns the paths that already exist. A path that cannot
// be checked is reported as existing, so the caller refuses rather than
// guesses.
func existingPaths(paths ...string) []string {
	var existing []string
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			existing = append(existing, p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			existing = append(existing, p)
		}
	}
	return existing
}

// writeNewFile creates path with body and mode, and fails if path
// already exists. O_EXCL does not follow a symlink at path, and the mode
// is set explicitly so the process umask cannot widen or narrow it.
func writeNewFile(path string, body []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing file %s", path)
		}
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("closing %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("setting permissions on %s: %w", path, err)
	}
	return nil
}

func parseFirstCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("certificate PEM has no CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing certificate: %w", err)
	}
	return cert, nil
}
