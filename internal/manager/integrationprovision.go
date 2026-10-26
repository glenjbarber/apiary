package manager

// ADR-0174: provisioning the certificate integration to a joining Comb.
//
// The integration is what lets a joining Comb obtain and renew
// certificates of its own, rather than living on the prepared one
// until it expires. It travels over the join channel, in the existing
// sealed credential bundle, and only to a Comb that was PREPARED FOR.
//
// WHY ONLY A PREPARED COMB. A zone-scoped Cloudflare token can change
// what every host in that zone resolves to. Handing it to every Comb
// in a Colony would multiply the blast radius of a single compromise
// for the benefit of Combs that will never issue a certificate.
//
// So the RESERVATION is the request. A Comb with a prepared
// certificate waiting for it is a Comb that is going to need to
// renew one; a Comb with no reservation gets no credential, and the
// bundle simply carries none.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/certintegration"
	"github.com/glenjbarber/apiary/internal/joincredential"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// integrationForPreparedJoin assembles the certificate integration for
// a joining Comb, or returns nil when the Comb was not prepared for.
//
// A nil integration is NOT an error: it is the ordinary case for
// most Combs, and the bundle that carries it stays at version 1.
//
// The token is read from THIS Comb's saved integration file. It is
// never read from replicated state, and it never appears anywhere
// except inside the sealed bundle.
func (s *Server) integrationForPreparedJoin(ctx context.Context, pending *internalpb.PendingJoinRequest) (*joincredential.Integration, error) {
	// A reservation bound to THIS request is the signal that the
	// joining Comb is going to need its own certificates.
	// A nil raft client means there are no reservations - which is
	// the same answer as "no reservation for this request".
	if s.raft == nil {
		return nil, nil
	}
	reservations, err := s.raft.ListCertificateReservationsLocal(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the prepared certificate reservations: %w", err)
	}
	prepared := false
	for _, reservation := range reservations.GetReservations() {
		if reservation.GetRequestId() == pending.GetRequestId() && reservation.GetNodeId() == pending.GetNodeId() {
			prepared = true
			break
		}
	}
	if !prepared {
		return nil, nil
	}

	// The saved integration on this Comb holds the token and the
	// public settings.
	saved, err := s.loadSavedIntegration()
	if err != nil {
		return nil, err
	}
	if saved == nil {
		// The Comb was prepared for, but this Comb has no integration
		// saved - so there is nothing to provision. The joining Comb
		// will stay on the prepared certificate, which is a working
		// certificate, and the operator can provision it by hand.
		return nil, nil
	}
	// The TOKEN is in its own 0600 file, which is where the
	// walkthrough puts it and where it belongs: a file that holds
	// nothing but the credential.
	token, err := s.readSavedCloudflareToken()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, nil
	}

	// The ACME directory and account key file are the JOINING Comb's
	// own paths, not this Comb's: the account key is generated on the
	// joiner and never travels, and the two Combs must not share one
	// account key.
	acmeDirectory := certintegration.DefaultProvisionedACMEDirectory
	accountKeyFile := filepath.Join(acmeDirectory, certintegration.DefaultProvisionedACMEAccountKeyName)

	return &joincredential.Integration{
		CloudflareToken:    token,
		CloudflareZoneID:   saved.ZoneID,
		CloudflareZoneName: saved.ZoneName,
		AllowedSuffix:      saved.AllowedSuffix,
		ACMEDirectory:      acmeDirectory,
		ACMEAccountKeyFile: accountKeyFile,
		ACMEDirectoryURL:   saved.ACMEDirectoryURL,
		ACMEEmail:          saved.ACMEEmail,
	}, nil
}

type savedIntegration struct {
	ZoneID           string `json:"zone_id"`
	ZoneName         string `json:"zone_name"`
	AllowedSuffix    string `json:"allowed_suffix"`
	ACMEDirectoryURL string `json:"acme_directory_url"`
	ACMEEmail        string `json:"acme_email"`
}

// loadSavedIntegration reads this Comb's saved integration.
//
// It returns nil, nil when there is no saved integration, because
// "this Comb has not been set up for certificates" is the ordinary
// case and not a failure.
func (s *Server) loadSavedIntegration() (*savedIntegration, error) {
	path := s.savedIntegrationPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the saved certificate integration: %w", err)
	}
	var saved savedIntegration
	if err := decodeSavedIntegration(data, &saved); err != nil {
		return nil, fmt.Errorf("the saved certificate integration at %s is malformed: %w", path, err)
	}
	return &saved, nil
}

// savedIntegrationPath is where this Comb's saved integration lives.
//
// It is beside the token, in the same protected directory, and it is
// 0600 because it carries the token.
func (s *Server) savedIntegrationPath() string {
	dir := s.savedCertificateDirectory()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, savedIntegrationFileName)
}

// savedIntegrationFileName is the file name of the saved integration.
const savedIntegrationFileName = "integration.json"

// savedCertificateDirectory is where this Comb keeps its certificate
// integration and its token.
//
// It comes from the node configuration, so the same directory is
// named by the configuration that points at it.
func (s *Server) savedCertificateDirectory() string {
	if s.nodeConfig == nil {
		return ""
	}
	cfg, err := s.nodeConfig.Load()
	if err != nil {
		return ""
	}
	if cfg.CloudflareTokenFile == "" {
		return ""
	}
	return filepath.Dir(cfg.CloudflareTokenFile)
}

// installProvisionedIntegration writes a provisioned integration onto
// the JOINING Comb.
//
// It is the joiner's half, and it is where the token lands.
//
// The token is written FIRST, to a 0600 root-owned file, because it is
// the secret and everything else is a path that names it. And an
// existing token is NEVER overwritten: a Comb that already has an
// integration keeps it, because a join must not silently replace a
// credential the operator set up.
func installProvisionedIntegration(integration *joincredential.Integration, managerConfigPath string) error {
	if integration == nil {
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing the certificate integration writes root-owned files; rerun this command from a root shell")
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
	tokenPath := filepath.Join(directory, provisionedTokenFileName)
	if err := writeSecretFile(tokenPath, []byte(integration.CloudflareToken+"\n")); err != nil {
		return fmt.Errorf("installing the provisioned Cloudflare token: %w", err)
	}

	// The ACME directory and the account key file are the joining
	// Comb's own.
	acmeDirectory := integration.ACMEDirectory
	if acmeDirectory == "" {
		acmeDirectory = certintegration.DefaultProvisionedACMEDirectory
	}
	accountKeyFile := integration.ACMEAccountKeyFile
	if accountKeyFile == "" {
		accountKeyFile = filepath.Join(acmeDirectory, certintegration.DefaultProvisionedACMEAccountKeyName)
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

// provisionedTokenFileName is where a provisioned Comb's Cloudflare
// token is written.
const provisionedTokenFileName = "cloudflare-token"

// defaultCertificateDirectory is where a provisioned Comb keeps its
// token when its configuration does not name a directory.
func defaultCertificateDirectory(cfg nodeconfig.Config) string {
	if cfg.CloudflareTokenFile != "" {
		return filepath.Dir(cfg.CloudflareTokenFile)
	}
	return certintegration.DefaultProvisionedACMEDirectory
}

// writeSecretFile writes a secret to a 0600 root-owned file.
//
// The mode is the security property: a zone-scoped Cloudflare token
// that anyone on the host can read is a token anyone on the host can
// spend. The mode is set BEFORE the rename, so there is no window in
// which the file exists under its final name with a looser mode.
func writeSecretFile(path string, data []byte) error {
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

// decodeSavedIntegration decodes a saved integration record.
func decodeSavedIntegration(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// the response type is referenced so the proto change is checked at
// compile time rather than by a comment.
var _ = rpcpb.GetJoinRequestStatusResponse{}

// readSavedCloudflareToken reads this Comb's saved Cloudflare token.
//
// The token is in its own file because a file that holds only the
// credential is a file whose permissions can be reasoned about. A
// shared file holding the token AND the zone would have to be 0600
// for the token's sake, which would make the PUBLIC settings
// unreadable by anything but root.
func (s *Server) readSavedCloudflareToken() (string, error) {
	if s.nodeConfig == nil {
		return "", nil
	}
	cfg, err := s.nodeConfig.Load()
	if err != nil {
		return "", fmt.Errorf("reading this Comb's managerd configuration: %w", err)
	}
	if cfg.CloudflareTokenFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(cfg.CloudflareTokenFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading the saved Cloudflare token: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
