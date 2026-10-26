// Package certintegration provisions a certificate integration onto a
// Comb.
//
// It is its own package because BOTH sides of a join need it: the
// preparing Comb assembles the integration and puts it in the sealed
// bundle, and the joining Comb installs it. Putting the installation
// in the manager package would have made the joiner depend on the
// manager, which drags raft, bbolt and the cluster packages into a
// binary an operator runs during an incident.
//
// The installer's rules are the security properties:
//
//   - THE TOKEN IS THE SECRET, and it lands in a 0600 root-owned file
//     before anything else happens.
//
//   - AN EXISTING TOKEN IS NEVER REPLACED. A Comb that already has an
//     integration keeps it, because a join must not silently replace a
//     credential the operator set up.
//
//   - THE ACME ACCOUNT KEY IS NEVER TRANSFERRED. It is generated on
//     the joining Comb, because two Combs sharing one ACME account key
//     would mean one compromised Comb could issue for the other's
//     account.

package certintegration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/glenjbarber/apiary/internal/joincredential"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// Install writes a provisioned integration onto
// the JOINING Comb.
//
// It is the joiner's half, and it is where the token lands.
//
// The token is written FIRST, to a 0600 root-owned file, because it is
// the secret and everything else is a path that names it. And an
// existing token is NEVER overwritten: a Comb that already has an
// integration keeps it, because a join must not silently replace a
// credential the operator set up.
func Install(integration *joincredential.Integration, managerConfigPath string) error {
	return InstallWith(integration, managerConfigPath, CheckRoot)
}

// InstallWith is Install, with the root check as a parameter.
//
// The root check is a POLICY check, not a capability one: the install
// itself works perfectly well without root, because it writes into the
// caller's own temporary directory. So a test can exercise the install
// without being root, which is the difference between a test that
// runs and a test that is skipped.
func InstallWith(integration *joincredential.Integration, managerConfigPath string, checkRoot func() error) error {
	if integration == nil {
		return nil
	}
	if err := checkRoot(); err != nil {
		return err
	}
	if strings.TrimSpace(integration.CloudflareToken) == "" {
		return fmt.Errorf("the provisioned integration carries no Cloudflare token")
	}
	manager := &nodeconfig.Manager{Path: managerConfigPath}
	cfg, err := manager.Load()
	if err != nil {
		return fmt.Errorf("reading this Comb's managerd configuration: %w", err)
	}

	// An existing token is never replaced.
	if cfg.CloudflareTokenFile != "" {
		if _, statErr := os.Stat(cfg.CloudflareTokenFile); statErr == nil {
			return fmt.Errorf("this Comb already has a Cloudflare token at %s; refusing to replace it. Remove it deliberately if you mean to change it.",
				cfg.CloudflareTokenFile)
		}
	}

	// The token goes into the directory the configuration names, or
	// the default one.
	directory := defaultCertificateDirectory(cfg)
	tokenPath := filepath.Join(directory, ProvisionedTokenFileName)
	if err := WriteSecretFile(tokenPath, []byte(integration.CloudflareToken+"\n")); err != nil {
		return fmt.Errorf("installing the provisioned Cloudflare token: %w", err)
	}

	// The ACME directory and the account key file are the joining
	// Comb's own.
	acmeDirectory := integration.ACMEDirectory
	if acmeDirectory == "" {
		acmeDirectory = DefaultProvisionedACMEDirectory
	}
	accountKeyFile := integration.ACMEAccountKeyFile
	if accountKeyFile == "" {
		accountKeyFile = filepath.Join(acmeDirectory, DefaultProvisionedACMEAccountKeyName)
	}

	// The public settings, recorded so the issuance path knows the
	// zone, the suffix and the ACME account.
	cfg.CloudflareTokenFile = tokenPath
	cfg.CloudflareZoneID = integration.CloudflareZoneID
	cfg.PreparedCertificateSuffix = integration.AllowedSuffix
	cfg.AcmeDirectory = acmeDirectory
	cfg.AcmeAccountKeyFile = accountKeyFile
	if integration.ACMEDirectoryURL != "" {
		cfg.AcmeDirectoryURL = integration.ACMEDirectoryURL
	}
	if integration.ACMEEmail != "" {
		cfg.AcmeEmail = integration.ACMEEmail
	}
	if err := manager.Save(cfg); err != nil {
		return fmt.Errorf("saving this Comb's managerd configuration: %w", err)
	}
	return nil
}

// defaultCertificateDirectory is where a provisioned Comb keeps its
// token when its configuration does not name a directory.
func defaultCertificateDirectory(cfg nodeconfig.Config) string {
	if cfg.CloudflareTokenFile != "" {
		return filepath.Dir(cfg.CloudflareTokenFile)
	}
	return DefaultProvisionedACMEDirectory
}

// writeSecretFile writes a secret to a 0600 root-owned file.
//
// The mode is the security property: a zone-scoped Cloudflare token
// that anyone on the host can read is a token anyone on the host can
// spend. The mode is set BEFORE the rename, so there is no window in
// which the file exists under its final name with a looser mode.
func WriteSecretFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".apiary-secret-*")
	if err != nil {
		return fmt.Errorf("staging %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("staging %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("staging %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("staging %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("installing %s: %w", path, err)
	}
	return nil
}

// DefaultProvisionedACMEDirectory is where a provisioned Comb keeps
// its certificates.
//
// It is a directory of its own, not the preparing Comb's: the two
// Combs' certificate inventories are separate, and a shared one would
// let one Comb's renewal touch another's files.
const DefaultProvisionedACMEDirectory = "/usr/local/etc/apiary/acme"

// DefaultProvisionedACMEAccountKeyName is the file name of a
// provisioned Comb's own ACME account key.
//
// The key is GENERATED on the joining Comb, never transferred. Two
// Combs sharing one ACME account key would mean one compromised Comb
// could issue for the other's account.
const DefaultProvisionedACMEAccountKeyName = "account.key"

// ProvisionedTokenFileName is where a provisioned Comb's Cloudflare
// token is written.
const ProvisionedTokenFileName = "cloudflare-token"

// CheckRoot refuses an install without root.
//
// The install writes the Cloudflare token and the node configuration,
// which are root-owned files on this Comb: managerd reads the
// configuration as root, and a token readable by the operator would be
// a token any local user could spend.
func CheckRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing the certificate integration writes root-owned files; rerun this command from a root shell")
	}
	return nil
}
