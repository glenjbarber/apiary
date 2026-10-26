package main

// apiaryctl cert-setup is the guided walkthrough for configuring
// Cloudflare and obtaining a certificate for a Comb (ADR-0172).
//
// WHY A COMMAND AND NOT A PAGE. ADR-0136 says apiaryctl exists for
// exactly this: an operator must be able to type the thing on a Comb
// that has no browser and no checkout. A joining Comb is headless, and
// the first node of a Colony may be too. The Machine page's ACME panel
// remains and remains a page - it is the right surface for a browser at
// a desk - but a guided flow that only exists in the web UI is a guided
// flow the operator cannot reach in the situation it was designed for.
//
// THE THREE RULES THIS FILE EXISTS TO ENFORCE:
//
//  1. A CREDENTIAL IS NEVER AN ARGUMENT, NEVER ECHOED, NEVER PRINTED.
//     The Cloudflare token is read from the terminal without echo and
//     written to a 0600 root-owned file. It is not a flag, not an
//     environment variable, and not in the output. The walkthrough
//     reports the token's PERMISSIONS, never the token.
//
//  2. ISSUANCE IS SEPARATE FROM ANY PERSISTENT DNS CHANGE. The
//     walkthrough may optionally create a record for the name, but only
//     after the operator explicitly selects it and sees exactly what
//     will be created. It never silently exposes a service and never
//     enables the Cloudflare proxy.
//
//  3. A NAME IS CHECKED AGAINST THE ALLOWED SUFFIX BEFORE ANYTHING IS
//     REQUESTED. The suffix is what stops the saved integration from
//     issuing a certificate for a name the operator never meant to
//     give this Colony.
//
// The flow is MENU-DRIVEN, with a flag answering each step, so the
// same code path serves a human at a console and a script. A command
// that only works interactively cannot be tested.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/cloudflare"
)

const certSetupUsage = `usage: apiaryctl cert-setup [step] [flags]

Guides an operator through configuring Cloudflare and obtaining a
certificate for this Comb. Every step prompts, and every step accepts a
flag that answers it, so the same flow serves a console and a script.

STEPS
  setup       enter and validate the Cloudflare integration, then issue
              a certificate for this Comb's own name
  prepare     prepare a certificate for ANOTHER Comb that has not joined
              yet, reusing the saved integration
  status      report the saved integration and the certificates this
              Comb holds
  help        this message

FLAGS
  --zone ZONE        the Cloudflare zone (name or ID) to work in
  --name NAME        the FQDN to issue for
  --suffix SUFFIX    the allowed naming suffix, e.g. .lab2.apiary.work
  --dns-record       also create a DNS record for the name (off by
                     default). The record is shown before it is created
  --non-interactive   never prompt; fail instead if a step would prompt
  --token-file FILE  read the Cloudflare token from FILE instead of
                     prompting (for automation; the file is 0600)

The Cloudflare token is read without echo and written to a 0600,
root-owned file. It is never an argument, never echoed, and never
printed. The walkthrough reports what the token may do, not the token.

This must run as root: it writes the token file and the certificates.
`

const (
	// defaultCertSetupDir is where the saved integration and the
	// certificates live.
	//
	// The token file is here rather than in managerd.json because that
	// file is configuration that gets rendered, copied and backed up.
	// A credential belongs in a 0600 file that only the issuance path
	// reads.
	defaultCertSetupDir = "/usr/local/etc/apiary/certs"

	// defaultCloudflareTokenFile is the saved Cloudflare API token.
	defaultCloudflareTokenFile = "cloudflare-token"

	// defaultIntegrationFile records the PUBLIC half of the saved
	// integration: the zone, the allowed suffix, and the ACME
	// settings. It carries no secret.
	defaultIntegrationFile = "integration.json"
)

// certSetupIntegration is the PUBLIC record of a saved Cloudflare
// integration.
//
// It is deliberately separate from the token: this file names the zone
// and the suffix and the ACME account's contact, and it may be copied
// between Combs. The token file may not.
type certSetupIntegration struct {
	// ZoneID is the Cloudflare zone, and ZoneName its human name.
	ZoneID   string `json:"zone_id"`
	ZoneName string `json:"zone_name"`
	// AllowedSuffix is the naming suffix this integration may issue
	// for. Empty means no suffix restriction, which is why the
	// walkthrough asks for one and refuses to proceed without it
	// unless the operator explicitly says otherwise.
	AllowedSuffix string `json:"allowed_suffix"`
	// ACMEDirectoryURL is the ACME server. Empty means Let's
	// Encrypt's production directory.
	ACMEDirectoryURL string `json:"acme_directory_url,omitempty"`
	// ACMEEmail is the contact address registered with the ACME
	// account.
	ACMEEmail string `json:"acme_email,omitempty"`
	// ACMEAccountKeyFile is where the ACME account key is kept. It
	// names a file; it does not carry the key.
	ACMEAccountKeyFile string `json:"acme_account_key_file,omitempty"`
}

// certSetupState is what the walkthrough needs to know, resolved from
// flags and prompts.
type certSetupState struct {
	// Token is the Cloudflare API token, read without echo.
	Token string
	// TokenFile is where the token is written.
	TokenFile string
	// IntegrationFile is where the public integration is written.
	IntegrationFile string
	// Zone is the zone name or ID the operator gave.
	Zone string
	// Name is the FQDN to issue for.
	Name string
	// Suffix is the allowed naming suffix.
	Suffix string
	// Email is the ACME contact address.
	Email string
	// DNSRecord is the operator's explicit choice to create a
	// persistent DNS record for the name.
	DNSRecord bool
	// NonInteractive refuses to prompt.
	NonInteractive bool
	// PreparedValidity is how long a PREPARED certificate stays
	// deliverable. It is bounded, because an unbounded preparation holds
	// a prepared key indefinitely.
	PreparedValidity time.Duration
	// Out and Err are where the walkthrough writes.
	Out io.Writer
	Err io.Writer
	// In is where prompts are read from.
	In io.Reader
}

// runCertSetup is the entry point for `apiaryctl cert-setup`.
func runCertSetup(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, certSetupUsage)
		return 2
	}
	switch args[0] {
	case "setup":
		return runCertSetupSetup(args[1:])
	case "prepare":
		return runCertSetupPrepare(args[1:])
	case "status":
		return runCertSetupStatus(args[1:])
	case "help", "-h", "--help":
		fmt.Print(certSetupUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "apiaryctl cert-setup: unknown step %q\n\n%s", args[0], certSetupUsage)
		return 2
	}
}

// certSetupFlags are the flags every step shares.
type certSetupFlags struct {
	zone       string
	name       string
	suffix     string
	email      string
	dir        string
	tokenFile  string
	dnsRecord  bool
	nonInterac bool
}

// parseCertSetupFlags parses the shared flags.
func parseCertSetupFlags(fs *flag.FlagSet, args []string) (*certSetupFlags, error) {
	f := &certSetupFlags{}
	fs.StringVar(&f.zone, "zone", "", "the Cloudflare zone (name or ID)")
	fs.StringVar(&f.name, "name", "", "the FQDN to issue for")
	fs.StringVar(&f.suffix, "suffix", "", "the allowed naming suffix")
	fs.StringVar(&f.email, "email", "", "the ACME contact address")
	fs.StringVar(&f.dir, "dir", defaultCertSetupDir, "directory holding the integration and the certificates")
	fs.StringVar(&f.tokenFile, "token-file", "", "read the Cloudflare token from this file instead of prompting")
	fs.BoolVar(&f.dnsRecord, "dns-record", false, "also create a DNS record for the name")
	fs.BoolVar(&f.nonInterac, "non-interactive", false, "never prompt; fail instead if a step would prompt")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return f, nil
}

// prompt asks a question and returns the answer.
//
// It refuses to prompt in non-interactive mode, because a command that
// silently waits for input that will never come is worse than one that
// says what it needed and stops.
func (s *certSetupState) prompt(question, def string) (string, error) {
	if s.NonInteractive {
		if def != "" {
			return def, nil
		}
		return "", fmt.Errorf("this step needs a value and --non-interactive was given; pass the matching flag")
	}
	if def != "" {
		fmt.Fprintf(s.Out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(s.Out, "%s: ", question)
	}
	reader := bufio.NewReader(s.In)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading the answer: %w", err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// promptSecret reads a secret without echoing it.
//
// The terminal is put into no-echo mode for the duration of the read.
// If the terminal cannot be put into that mode, the walkthrough
// REFUSES rather than falling back to an echoed read: a token printed
// to the screen is in the operator's scrollback, in a screen recording,
// and possibly in a photograph of the screen.
func (s *certSetupState) promptSecret(question string) (string, error) {
	if s.NonInteractive {
		return "", fmt.Errorf("the Cloudflare token must be supplied with --token-file when --non-interactive is given")
	}
	fmt.Fprintf(s.Out, "%s (not echoed): ", question)
	secret, err := readSecret(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("reading the token: %w", err)
	}
	fmt.Fprintln(s.Out)
	return strings.TrimSpace(secret), nil
}

// nameAllowedAgainstSuffix reports whether name is inside the allowed
// suffix.
//
// The check is what stops the saved integration from issuing a
// certificate for a name the operator never meant to give this Colony.
// It is done BEFORE anything is requested, so a typo costs nothing.
//
// An empty suffix allows nothing: the walkthrough asks for a suffix
// and an empty one is a refusal, not a wildcard. A wildcard suffix
// would let the integration issue for any name in the zone, which is
// the "shared zone token" consequence ADR-0169's writer recorded and
// is exactly what the naming suffix exists to bound.
func nameAllowedAgainstSuffix(name, suffix string) error {
	name = strings.TrimSpace(strings.ToLower(name))
	suffix = strings.TrimSpace(strings.ToLower(suffix))
	if name == "" {
		return errors.New("the name is empty")
	}
	if suffix == "" {
		return errors.New("no allowed naming suffix is set. The suffix is what stops this integration from issuing a certificate for a name you did not mean to give this Colony")
	}
	if suffix == "." {
		return errors.New("the allowed naming suffix must name a domain, not the whole zone")
	}
	if !strings.HasPrefix(suffix, ".") {
		suffix = "." + suffix
	}
	if !strings.HasSuffix(name, suffix) {
		return fmt.Errorf("%q is not inside the allowed suffix %q", name, suffix)
	}
	return nil
}

// runCertSetupStatus reports the saved integration.
//
// It is a READ, and it prints no secret: the integration record is
// public by construction, and the token file's existence is reported
// by path and mode rather than by content.
func runCertSetupStatus(args []string) int {
	fs := flag.NewFlagSet("apiaryctl cert-setup status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, certSetupUsage) }
	f, err := parseCertSetupFlags(fs, args)
	if err != nil {
		return 2
	}
	integration, err := loadCertSetupIntegration(filepath.Join(f.dir, defaultIntegrationFile))
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl cert-setup status: %v\n", err)
		return 1
	}
	fmt.Printf("Cloudflare integration:\n")
	fmt.Printf("  zone           %s (%s)\n", integration.ZoneName, integration.ZoneID)
	if integration.AllowedSuffix != "" {
		fmt.Printf("  allowed suffix %s\n", integration.AllowedSuffix)
	}
	if integration.ACMEEmail != "" {
		fmt.Printf("  acme contact   %s\n", integration.ACMEEmail)
	}
	if integration.ACMEDirectoryURL != "" {
		fmt.Printf("  acme server    %s\n", integration.ACMEDirectoryURL)
	}
	tokenPath := filepath.Join(f.dir, defaultCloudflareTokenFile)
	if info, statErr := os.Stat(tokenPath); statErr == nil {
		fmt.Printf("  token file     %s (mode %04o)\n", tokenPath, info.Mode().Perm())
	} else {
		fmt.Printf("  token file     %s (absent)\n", tokenPath)
	}
	return 0
}

// loadCertSetupIntegration reads the saved integration record.
func loadCertSetupIntegration(path string) (*certSetupIntegration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Cloudflare integration is saved at %s; run `apiaryctl cert-setup setup` first", path)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var integration certSetupIntegration
	if err := decodeJSON(data, &integration); err != nil {
		return nil, fmt.Errorf("the saved integration at %s is malformed: %w", path, err)
	}
	return &integration, nil
}

// saveCertSetupIntegration writes the public integration record.
//
// It is written 0644 because it carries no secret: it names the zone
// and the suffix and the ACME settings, and the operator may need to
// read it, copy it to another Comb, or read it back into this one.
func saveCertSetupIntegration(path string, integration *certSetupIntegration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	data, err := encodeJSON(integration)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// saveCloudflareToken writes the Cloudflare token, 0600.
//
// The mode is the security property: the token is a zone-scoped
// credential, and a world-readable one is a token anyone on the host
// can spend.
func saveCloudflareToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// WriteFile does not change the mode of an existing file, so the
	// chmod is not redundant: the file may already exist with a
	// looser mode, and this walkthrough's whole promise is that the
	// token is not readable by anyone else.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("securing %s: %w", path, err)
	}
	return nil
}

// verifyCloudflareToken checks the token against the zone and reports
// what it may do.
//
// The report names the zone and the account and never the token.
func verifyCloudflareToken(ctx context.Context, token, zone string) (cloudflare.TokenCheck, error) {
	zoneID := strings.TrimSpace(zone)
	// A zone NAME is resolved to its ID first, because the API's
	// permission checks are against IDs. A name that matches more than
	// one zone is refused rather than resolved to the first.
	if zoneID != "" && !strings.HasPrefix(zoneID, "z") || zoneID != "" && !isLikelyZoneID(zoneID) {
		found, err := cloudflare.FindZoneByName(ctx, token, zoneID)
		if err != nil {
			return cloudflare.TokenCheck{}, err
		}
		zoneID = found.ID
	}
	return cloudflare.VerifyToken(ctx, token, zoneID)
}

// isLikelyZoneID reports whether a string looks like a Cloudflare
// zone ID.
//
// Cloudflare's zone IDs are 32 hex characters. This is a heuristic
// used only to decide whether to try a NAME lookup or treat the value
// as an ID directly - and a value that is neither is resolved as a
// name, which produces a refusal naming the problem rather than a
// confusing API error.
func isLikelyZoneID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// certSetupTimeout bounds the walkthrough's network calls.
//
// The Cloudflare client has its own timeout; this bounds the whole
// step, so a walkthrough left unattended on a console cannot hang
// forever on a provider that never answers.
const certSetupTimeout = 2 * time.Minute
