package main

// The walkthrough's two issue steps.
//
// setup     configures the Cloudflare integration and issues a
//           certificate for THIS Comb.
// prepare   issues a certificate for ANOTHER Comb that has not joined
//           yet, reusing the saved integration.
//
// The two share almost everything, which is the point: the second is
// the first without a new credential, and a flow that had two
// separate code paths would be two sets of rules to keep in step.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/cloudflare"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// runCertSetupSetup configures the integration and issues for this
// Comb.
func runCertSetupSetup(args []string) int {
	fs := flag.NewFlagSet("apiaryctl cert-setup setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, certSetupUsage) }
	f, err := parseCertSetupFlags(fs, args)
	if err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl cert-setup setup: must run as root.\n\nIt writes the Cloudflare token and the certificates. Run it from a root shell, or with sudo.\n")
		return 1
	}

	state := &certSetupState{
		TokenFile:       filepath.Join(f.dir, defaultCloudflareTokenFile),
		IntegrationFile: filepath.Join(f.dir, defaultIntegrationFile),
		Zone:            f.zone,
		Name:            f.name,
		Suffix:          f.suffix,
		Email:           f.email,
		DNSRecord:       f.dnsRecord,
		NonInteractive:  f.nonInterac,
		Out:             os.Stdout,
		Err:             os.Stderr,
		In:              os.Stdin,
	}

	fmt.Fprintln(state.Out, "Apiary certificate setup.")
	fmt.Fprintln(state.Out, "")

	// STEP 1: the token.
	//
	// It is read from a file when one is given, and prompted for
	// otherwise. Either way it lands in a 0600 file and is never
	// printed.
	token, err := state.obtainToken(f.tokenFile)
	if err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup setup: %v\n", err)
		return 1
	}
	state.Token = token

	// STEP 2: the zone.
	zone, err := state.obtainZone()
	if err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup setup: %v\n", err)
		return 1
	}
	state.Zone = zone

	// STEP 3: validate the token against the zone, BEFORE anything is
	// requested. A mis-scoped token is refused here with the specific
	// permission it lacks, rather than failing partway through an ACME
	// order with a TXT record left behind.
	ctx, cancel := context.WithTimeout(context.Background(), certSetupTimeout)
	defer cancel()
	check, err := verifyCloudflareToken(ctx, state.Token, state.Zone)
	if err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup setup: %v\n", err)
		return 1
	}
	fmt.Fprintf(state.Out, "Token accepted: %s.\n", check.Detail)

	// STEP 4: the allowed naming suffix.
	suffix, err := state.obtainSuffix()
	if err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup setup: %v\n", err)
		return 1
	}
	state.Suffix = suffix

	// STEP 5: the name.
	name, err := state.obtainName()
	if err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup setup: %v\n", err)
		return 1
	}
	state.Name = name

	// STEP 6: the ACME contact.
	email, err := state.obtainEmail()
	if err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup setup: %v\n", err)
		return 1
	}
	state.Email = email

	// STEP 7: save the integration, then issue.
	integration := &certSetupIntegration{
		ZoneID:        check.ZoneID,
		ZoneName:      check.ZoneName,
		AllowedSuffix: state.Suffix,
		ACMEEmail:     state.Email,
	}
	if err := saveCertSetupIntegration(state.IntegrationFile, integration); err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup setup: %v\n", err)
		return 1
	}
	if err := saveCloudflareToken(state.TokenFile, state.Token); err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup setup: %v\n", err)
		return 1
	}
	fmt.Fprintf(state.Out, "Integration saved to %s.\n", state.IntegrationFile)
	fmt.Fprintf(state.Out, "Token saved to %s (mode 0600).\n", state.TokenFile)

	// The saved settings also go into this Comb's own configuration,
	// because that is where the ISSUANCE path reads them: managerd
	// holds the ACME issuer and the certificate directory, and it
	// needs to know the zone, the token's path and the allowed suffix
	// to issue anything.
	//
	// The TOKEN ITSELF is not written here. This configuration names
	// the token's PATH, and the path is what the issuance reads.
	if err := state.saveNodeConfig(check.ZoneID, integration.ZoneName, state.TokenFile); err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup setup: %v\n", err)
		return 1
	}
	fmt.Fprintln(state.Out, "This Comb's configuration now names the zone, the token file and the allowed suffix.")

	return state.issue(ctx, f, true)
}

// runCertSetupPrepare issues a certificate for another Comb that has
// not joined yet.
//
// It REUSES the saved integration: the token is read from the saved
// file, the zone and the suffix are read from the saved record, and
// the operator is not asked for the credential again. That reuse is
// the whole point of saving it.
func runCertSetupPrepare(args []string) int {
	fs := flag.NewFlagSet("apiaryctl cert-setup prepare", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, certSetupUsage) }
	f, err := parseCertSetupFlags(fs, args)
	if err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl cert-setup prepare: must run as root.\n\nIt writes the prepared certificate and key. Run it from a root shell, or with sudo.\n")
		return 1
	}

	state := &certSetupState{
		TokenFile:       filepath.Join(f.dir, defaultCloudflareTokenFile),
		IntegrationFile: filepath.Join(f.dir, defaultIntegrationFile),
		Zone:            f.zone,
		Name:            f.name,
		Suffix:          f.suffix,
		Email:           f.email,
		DNSRecord:       f.dnsRecord,
		NonInteractive:  f.nonInterac,
		Out:             os.Stdout,
		Err:             os.Stderr,
		In:              os.Stdin,
	}

	integration, err := loadCertSetupIntegration(state.IntegrationFile)
	if err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup prepare: %v\n", err)
		return 1
	}
	token, err := os.ReadFile(state.TokenFile)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(state.Err, "apiaryctl cert-setup prepare: no Cloudflare token is saved at %s. Run `apiaryctl cert-setup setup` first.\n", state.TokenFile)
			return 1
		}
		fmt.Fprintf(state.Err, "apiaryctl cert-setup prepare: reading the saved token: %v\n", err)
		return 1
	}
	state.Token = strings.TrimSpace(string(token))
	state.Suffix = integration.AllowedSuffix
	if state.Zone == "" {
		state.Zone = integration.ZoneName
	}
	if state.Email == "" {
		state.Email = integration.ACMEEmail
	}
	if state.Suffix == "" {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup prepare: the saved integration has no allowed naming suffix, so it cannot issue for any name.\n")
		return 1
	}

	name, err := state.obtainName()
	if err != nil {
		fmt.Fprintf(state.Err, "apiaryctl cert-setup prepare: %v\n", err)
		return 1
	}
	state.Name = name

	ctx, cancel := context.WithTimeout(context.Background(), certSetupTimeout)
	defer cancel()
	return state.issue(ctx, f, false)
}

// issue performs the issuance itself, for this Comb or for another.
//
// forThisComb is what distinguishes the two steps at the point where
// it matters: a certificate for THIS Comb is installed as this Comb's
// serving certificate, while a certificate for ANOTHER Comb is a
// PREPARED pair that the join flow will deliver.
func (s *certSetupState) issue(ctx context.Context, f *certSetupFlags, forThisComb bool) int {
	// The name is checked against the suffix BEFORE anything is
	// requested. A typo costs nothing this way.
	if err := nameAllowedAgainstSuffix(s.Name, s.Suffix); err != nil {
		fmt.Fprintf(s.Err, "apiaryctl cert-setup: %v\n", err)
		return 1
	}
	fmt.Fprintf(s.Out, "")
	fmt.Fprintf(s.Out, "About to request a certificate for %s in zone %s.\n", s.Name, s.Zone)

	// The optional DNS record is shown BEFORE it is created, and only
	// created if the operator asked for it. A persistent DNS change is
	// a visible consequence and belongs to an explicit choice.
	if s.DNSRecord {
		fmt.Fprintf(s.Out, "This will also create a DNS record for %s. It will NOT be proxied.\n", s.Name)
	}

	// The issuance itself is done by the managerd RPC, which owns the
	// ACME issuer and the certificate directory. The walkthrough does
	// not duplicate that path: there is one implementation of
	// issuance, and it is the one the Machine page uses too.
	fmt.Fprintf(s.Out, "Requesting the certificate. This can take a minute while the DNS challenge propagates.\n")
	if err := s.requestIssuance(ctx, forThisComb); err != nil {
		fmt.Fprintf(s.Err, "apiaryctl cert-setup: %v\n", err)
		return 1
	}
	fmt.Fprintf(s.Out, "Certificate issued and installed.\n")
	return 0
}

// obtainToken reads the Cloudflare token, from a file or from the
// terminal.
func (s *certSetupState) obtainToken(tokenFile string) (string, error) {
	if tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("reading the token file %s: %w", tokenFile, err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("the token file %s is empty", tokenFile)
		}
		return token, nil
	}
	return s.promptSecret("Cloudflare API token")
}

// obtainZone asks for the zone, offering the ones the token can see.
func (s *certSetupState) obtainZone() (string, error) {
	if s.Zone != "" {
		return s.Zone, nil
	}
	// The visible zones are offered, because picking from a list is
	// easier than remembering a zone ID.
	ctx, cancel := context.WithTimeout(context.Background(), certSetupTimeout)
	defer cancel()
	zones, err := cloudflare.ListZones(ctx, s.Token)
	if err != nil {
		// The list is a convenience, not a requirement: a token that
		// can see its zone but not list zones is unusual but legal.
		fmt.Fprintf(s.Err, "Could not list the zones this token can see (%v).\n", err)
		fmt.Fprintf(s.Err, "Enter the zone NAME or ID instead.\n")
		return s.prompt("Cloudflare zone", "")
	}
	if len(zones) == 0 {
		fmt.Fprintln(s.Err, "This token can see no zones. Enter the zone ID anyway if it is scoped to one.")
		return s.prompt("Cloudflare zone ID", "")
	}
	fmt.Fprintln(s.Out, "Zones this token can see:")
	for _, z := range zones {
		fmt.Fprintf(s.Out, "  %s  (%s)\n", z.Name, z.ID)
	}
	return s.prompt("Cloudflare zone", zones[0].Name)
}

// obtainSuffix asks for the allowed naming suffix.
func (s *certSetupState) obtainSuffix() (string, error) {
	if s.Suffix != "" {
		return s.Suffix, nil
	}
	answer, err := s.prompt("Allowed naming suffix (for example .lab2.apiary.work)", "")
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", fmt.Errorf("no allowed naming suffix was given. The suffix is what stops this integration from issuing for a name you did not mean to give this Colony")
	}
	if !strings.HasPrefix(answer, ".") {
		answer = "." + answer
	}
	return answer, nil
}

// obtainName asks for the FQDN.
func (s *certSetupState) obtainName() (string, error) {
	if s.Name != "" {
		return s.Name, nil
	}
	return s.prompt("FQDN to issue for", "")
}

// obtainEmail asks for the ACME contact address.
//
// It is OPTIONAL: Let's Encrypt does not require one, and the walkthrough
// says so rather than insisting.
func (s *certSetupState) obtainEmail() (string, error) {
	if s.Email != "" {
		return s.Email, nil
	}
	fmt.Fprintln(s.Out, "The ACME contact address is optional; Let's Encrypt does not require one.")
	return s.prompt("ACME contact email", "")
}

// requestIssuance performs the issuance through managerd.
//
// It is a separate method so the walkthrough's flow reads as a
// sequence of decisions and this one call is the only place that
// reaches for the network.
func (s *certSetupState) requestIssuance(ctx context.Context, forThisComb bool) error {
	return s.issueThroughManagerd(ctx, forThisComb)
}

// issueTimeout is the bound on the issuance call.
//
// Issuance publishes a DNS challenge and waits for it to propagate, so
// it is slower than an ordinary RPC. The bound is generous enough for
// a real propagation and short enough that a stuck walkthrough does
// not hang on a console.
const issueTimeout = 5 * time.Minute

// saveNodeConfig records the integration's settings in this Comb's own
// configuration.
//
// The TOKEN is not among them. This records the token's PATH, and the
// path is what the issuance path reads - so the credential itself
// stays in one 0600 file that only the issuance reads, and never
// reaches a configuration file that gets rendered, copied or backed
// up.
//
// The existing values are preserved: this touches only the three
// fields the integration owns.
func (s *certSetupState) saveNodeConfig(zoneID, zoneName, tokenFile string) error {
	manager := &nodeconfig.Manager{Path: nodeconfig.DefaultPath}
	cfg, err := manager.Load()
	if err != nil {
		return fmt.Errorf("reading this Comb's managerd configuration: %w", err)
	}
	cfg.CloudflareZoneID = zoneID
	cfg.CloudflareTokenFile = tokenFile
	cfg.PreparedCertificateSuffix = s.Suffix
	if err := manager.Save(cfg); err != nil {
		return fmt.Errorf("saving this Comb's managerd configuration: %w", err)
	}
	return nil
}
