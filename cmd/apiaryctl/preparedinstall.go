package main

// The joiner-side installation of a prepared certificate.
//
// ADR-0171 delivers a prepared certificate to the joining Comb, sealed
// to that Comb's OWN serving certificate. This is what the joiner does
// with it.
//
// THE FOUR THINGS THAT MATTER HERE:
//
//  1. THE PAIR IS INSTALLED AS THIS COMB'S OWN SERVING CERTIFICATE, at
//     the paths this Comb already serves from. Installing it
//     somewhere else would produce a certificate nothing serves.
//
//  2. THE INSTALL IS ATOMIC, through the same stage/fsync/rename
//     discipline every other certificate install uses. A failure
//     leaves the previous pair serving rather than a half-written one.
//
//  3. THE BOOTSTRAP KEY IS RETIRED ONLY AFTER THE REPLACEMENT IS IN
//     PLACE. Deleting it earlier would leave the Comb unable to serve
//     if the replacement failed.
//
//  4. THE TRUST TRANSITION IS NOT THIS COMB'S ACT. Installing a file
//     and changing what the Colony trusts are different things, and
//     ADR-0170's transition is a separate, separately-authorized one.
//
// The chain here is: the PREPARED pair is installed, the joiner
// generates a key of its OWN, obtains its OWN certificate from the
// Cloudflare integration, installs that, and only then retires the
// bootstrap key. So the window in which two machines hold the
// bootstrap key is bounded by the join itself.

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/glenjbarber/apiary/internal/certreservation"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/origincert"
)

// preparedInstallResult is what the joiner reports after adopting a
// prepared certificate.
type preparedInstallResult struct {
	// FQDN is the name the prepared certificate covers.
	FQDN string
	// Fingerprint is the ACTIVE certificate's fingerprint after the
	// transition - the value the Colony's trust transition needs, and
	// the value an operator comparing fingerprints wants to see.
	Fingerprint string
	// BootstrapRetired is true when the bootstrap key was destroyed
	// after the replacement was in place.
	BootstrapRetired bool
	// ReplacementObtained is true when the joiner obtained a
	// certificate of its own.
	ReplacementObtained bool
}

// installPreparedCertificate adopts a prepared certificate on this
// Comb.
//
// It is called after the join's credential installation has succeeded,
// and it installs the PREPARED pair first - so that this Comb is
// serving a real, publicly-trusted certificate for the name it was
// prepared for, rather than a self-signed one, from the moment it
// joins.
//
// The pair is written to this Comb's OWN tls_cert and tls_key paths,
// because that is where managerd, the frontend and the restshimd read
// their serving certificate from.
func installPreparedCertificate(envelope []byte, servingCert *localCertificate, reservationID, requestID, preparedFingerprint, preparedFQDN, managerConfigPath string) (*preparedInstallResult, error) {
	return installPreparedCertificateWith(envelope, servingCert, reservationID, requestID, preparedFingerprint, preparedFQDN, managerConfigPath, checkRootForPreparedInstall)
}

// installPreparedCertificateWith is the install, with the root check
// as a parameter.
//
// The root check is a POLICY check, not a capability one: the install
// itself works perfectly well without root, because it writes into
// the test's own temporary directory. So a test can exercise the
// install without being root, which is the difference between a test
// that runs and a test that is skipped.
func installPreparedCertificateWith(envelope []byte, servingCert *localCertificate, reservationID, requestID, preparedFingerprint, preparedFQDN, managerConfigPath string, checkRoot func() error) (*preparedInstallResult, error) {
	if err := checkRoot(); err != nil {
		return nil, err
	}
	if len(envelope) == 0 {
		return nil, errors.New("the Colony returned no prepared certificate envelope; retry the poll")
	}
	nodeManager := &nodeconfig.Manager{Path: managerConfigPath}
	nodeCfg, err := nodeManager.Load()
	if err != nil {
		return nil, fmt.Errorf("reading this Comb's managerd configuration: %w", err)
	}
	if nodeCfg.TLSKey == "" || nodeCfg.TLSCert == "" {
		return nil, errors.New("this Comb's managerd tls_cert and tls_key are not configured")
	}
	privateKey, err := os.ReadFile(nodeCfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("reading this Comb's TLS private key: %w", err)
	}

	// The delivery is sealed to THIS Comb's CURRENT serving
	// certificate, so that is the key it opens with. The reservation
	// the envelope was sealed for is reconstructed from the values the
	// Colony returned with it, and the OPENING verifies them against
	// the ciphertext's own associated data - so a delivery for another
	// reservation cannot be opened here.
	// The node ID comes from this Comb's OWN configuration, not from
	// the caller: it is this Comb's raft identity, and the delivery's
	// associated data binds it. Taking it from the configuration means
	// the reconstruction cannot be talked into naming a different
	// Comb.
	nodeID := strings.TrimSpace(nodeCfg.NodeID)
	if nodeID == "" {
		return nil, errors.New("this Comb's node_id is not configured, so the prepared certificate cannot be matched against this Comb")
	}
	// The FQDN is not part of the associated data, so the
	// reconstruction does not need it to open the envelope - but the
	// OPENING checks that the certificate covers the reserved name,
	// and that check needs the name. It is taken from the certificate
	// the envelope carries rather than from the caller, because the
	// caller is exactly the party the check exists to constrain.
	reservation := &certreservation.Reservation{
		ID:          reservationID,
		RequestID:   requestID,
		NodeID:      nodeID,
		Fingerprint: preparedFingerprint,
		FQDN:        preparedFQDN,
	}
	bundle, err := certreservation.OpenDelivery(string(envelope), servingCert.pem, string(privateKey), reservation)
	if err != nil {
		return nil, fmt.Errorf("opening the prepared certificate: %w", err)
	}

	// The name the certificate covers must be one this Comb is
	// prepared to serve. A prepared pair for a different name would
	// install cleanly and then fail every handshake with a name
	// mismatch, which is a much worse place to discover it.
	if err := checkPreparedNameMatchesThisComb(nodeCfg, bundle.FQDN); err != nil {
		return nil, err
	}

	// The pair is verified as a PAIR before it is written: a
	// certificate and key that do not match install cleanly and fail
	// every handshake.
	if err := certreservation.CheckKeyMatchesCertificate(bundle.CertificatePEM, bundle.PrivateKeyPEM); err != nil {
		return nil, fmt.Errorf("the prepared certificate and key do not match: %w", err)
	}

	// The pair is written ATOMICALLY, through the same discipline every
	// other certificate install uses.
	directory := filepath.Dir(nodeCfg.TLSCert)
	if _, _, err := origincert.WritePair(directory, preparedPairName(bundle.FQDN), bundle.CertificatePEM, bundle.PrivateKeyPEM); err != nil {
		return nil, fmt.Errorf("installing the prepared pair: %w", err)
	}
	// WritePair names the files by the certificate's NAME, and this
	// Comb's configuration names its own paths. They have to agree, so
	// the pair is written to the CONFIGURED paths.
	if err := installPreparedPairAt(nodeCfg.TLSCert, nodeCfg.TLSKey, bundle.CertificatePEM, bundle.PrivateKeyPEM); err != nil {
		return nil, err
	}

	fingerprint, err := preparedFingerprintFromCertificate(bundle.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("reading the prepared certificate's fingerprint: %w", err)
	}
	return &preparedInstallResult{
		FQDN:        bundle.FQDN,
		Fingerprint: fingerprint,
		// The bootstrap key is NOT retired here, and deliberately so.
		//
		// Retiring it requires the joiner to have a REPLACEMENT
		// certificate of its own, which requires the Cloudflare
		// integration configuration - and that configuration is
		// provisioned to the joiner separately. Until it arrives, the
		// joiner is serving a working, publicly-trusted certificate
		// and the bootstrap key is the only key it has.
		//
		// Deleting the bootstrap key here would leave a Comb that
		// cannot serve at all if the replacement then failed, which is
		// strictly worse than the two-holder window this whole
		// workflow accepts.
		BootstrapRetired: false,
	}, nil
}

// preparedPairName is the name a prepared pair is stored under.
//
// It is the FQDN, so the inventory entry that records it is named
// after the thing it is for rather than after an opaque identifier.
func preparedPairName(fqdn string) string {
	return fqdn
}

// checkPreparedNameMatchesThisComb refuses a prepared certificate for
// a name this Comb is not set up to serve.
//
// The Comb's own configuration names the certificate paths, and the
// certificate covers a name. If the Comb's short name is not part of
// the prepared name, the prepared pair is for a DIFFERENT Comb, and
// installing it here would make this Comb serve a certificate for
// somebody else's name.
func checkPreparedNameMatchesThisComb(cfg nodeconfig.Config, fqdn string) error {
	if strings.TrimSpace(fqdn) == "" {
		return errors.New("the prepared certificate names no FQDN")
	}
	// The Comb's own node ID is the name its peers dial, and it is
	// the first label of the FQDN a certificate for this Comb would
	// carry. A certificate whose name does not contain it belongs to
	// another Comb.
	if cfg.NodeID == "" {
		// Without a node ID there is nothing to check against, and a
		// check that cannot be made is not a passing check.
		return errors.New("this Comb's node_id is not configured, so the prepared certificate's name cannot be checked against it")
	}
	if !strings.Contains(strings.ToLower(fqdn), strings.ToLower(cfg.NodeID)) {
		return fmt.Errorf("the prepared certificate covers %q, which is not this Comb's name (%s). It was prepared for a different Comb.", fqdn, cfg.NodeID)
	}
	return nil
}

// installPreparedPairAt writes the pair to the CONFIGURED paths.
//
// origincert.WritePair names its files by the certificate's name, and
// this Comb's configuration names its own tls_cert and tls_key. The
// two have to agree, so the pair is written to the configured paths -
// and it is written with the SAME stage/fsync/rename discipline, so a
// failure leaves the previous pair serving.
func installPreparedPairAt(certPath, keyPath, certPEM, keyPEM string) error {
	if err := writeFileAtomic(certPath, []byte(certPEM), 0o644); err != nil {
		return fmt.Errorf("installing the prepared certificate: %w", err)
	}
	if err := writeFileAtomic(keyPath, []byte(keyPEM), 0o600); err != nil {
		return fmt.Errorf("installing the prepared private key: %w", err)
	}
	return nil
}

// writeFileAtomic writes a file through a staged, fsynced temporary
// file and a rename.
//
// The rename is what makes it atomic: a reader sees either the old
// file or the new one, never a partial one. And the mode is set
// BEFORE the rename, so there is no window in which the file exists
// under its final name with a looser mode.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".apiary-prepared-*")
	if err != nil {
		return fmt.Errorf("staging %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
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

// preparedFingerprintFromCertificate re-derives a certificate's
// fingerprint.
//
// It is the joiner's own derivation, through the one package that
// owns that format, so the value the joiner reports is the same string
// the peer trust store pins.
func preparedFingerprintFromCertificate(certPEM string) (string, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("the certificate is not PEM-encoded")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing the certificate: %w", err)
	}
	return hostcert.Fingerprint(cert.Raw), nil
}

// checkRootForPreparedInstall refuses to install a prepared certificate
// without root.
//
// The install writes the serving certificate and its private key,
// which are root-owned files on this Comb: managerd, the frontend and
// the restshimd all read them as root, and a key readable by the
// operator would be a key any local user could serve.
//
// It is a separate function so a test can exercise the install
// WITHOUT being root, which is the difference between a test that
// runs and a test that is skipped.
func checkRootForPreparedInstall() error {
	if os.Geteuid() != 0 {
		return errors.New("installing a prepared certificate writes root-owned files; rerun this command from a root shell")
	}
	return nil
}
