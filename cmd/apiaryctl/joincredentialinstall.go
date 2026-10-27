package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/certintegration"
	"github.com/glenjbarber/apiary/internal/colonycainstall"
	"github.com/glenjbarber/apiary/internal/colonyleaf"
	"github.com/glenjbarber/apiary/internal/colonyleafinstall"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/joincredential"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

const defaultRestartGuardrailTokenPath = "/usr/local/etc/apiary/restart-guardrail-token"

func installApprovedJoinCredentials(envelope []byte, servingCert *localCertificate, requestID, managerConfigPath string) error {
	if os.Geteuid() != 0 {
		return errors.New("installing join credentials writes root-owned files; rerun this command from a root shell")
	}
	if len(envelope) == 0 {
		return errors.New("the Colony approved this join but returned no sealed credential bundle; retry the poll or inspect the Colony leader's restart-guardrail token")
	}
	nodeManager := &nodeconfig.Manager{Path: managerConfigPath}
	nodeCfg, err := nodeManager.Load()
	if err != nil {
		return fmt.Errorf("reading this Comb's managerd configuration: %w", err)
	}
	if nodeCfg.TLSKey == "" {
		return errors.New("this Comb's managerd tls_key is not configured")
	}
	privateKey, err := os.ReadFile(nodeCfg.TLSKey)
	if err != nil {
		return fmt.Errorf("reading this Comb's TLS private key: %w", err)
	}
	identity, err := localJoinIdentity("", "")
	if err != nil {
		return err
	}
	plain, err := joincredential.Open(envelope, []byte(servingCert.pem), privateKey,
		joincredential.AssociatedData(requestID, identity.nodeID, servingCert.fingerprint))
	if err != nil {
		return fmt.Errorf("decrypting credentials for this Comb: %w", err)
	}
	var bundle joincredential.Bundle
	if err := json.Unmarshal(plain, &bundle); err != nil {
		return errors.New("decrypted credential bundle is malformed")
	}
	if bundle.RequestID != requestID || bundle.NodeID != identity.nodeID {
		return errors.New("decrypted credential bundle is bound to a different request or Comb")
	}
	// The version is 1, 2 or 3. A joiner that does not understand
	// version 3 REFUSES it rather than joining without the Colony CA
	// it needs - which is the whole point of putting the CA in the
	// bundle. Joining without it would produce a member the Colony
	// could not verify, and the failure would surface later and
	// further from its cause.
	if bundle.Version != 1 && bundle.Version != 2 && bundle.Version != 3 {
		return fmt.Errorf("decrypted credential bundle uses an unsupported version %d", bundle.Version)
	}
	block, _ := pem.Decode([]byte(servingCert.pem))
	if block == nil || hostcert.Fingerprint(block.Bytes) != servingCert.fingerprint {
		return errors.New("local serving certificate changed during join credential installation")
	}
	// ADR-0155 step 3: the Colony CA is installed BEFORE the
	// credentials, and it is the first thing that can fail on its own
	// terms.
	//
	// The CA is checked against the fingerprint the OPERATOR PINNED
	// in the join authorization - not against the fingerprint the
	// Colony put in the same bundle. Checking the Colony's
	// certificate against the Colony's own arithmetic would prove
	// nothing; the pinned value is the one a human compared by hand.
	//
	// So the CA is installed first, because a CA that does not match
	// what the operator pinned must stop the join BEFORE the
	// credentials are written, not after.
	if bundle.ColonyCA != nil {
		pinned := bundle.ColonyCA.PinnedFingerprint
		if pinned == "" {
			return errors.New("the Colony's CA came with no pinned fingerprint, so it cannot be trusted; this Comb will not join a Colony whose CA nobody compared by hand")
		}
		if err := colonycainstall.Install(bundle.ColonyCA, pinned, colonycainstall.DefaultCADir, time.Now()); err != nil {
			return fmt.Errorf("installing the Colony CA: %w", err)
		}
	}

	// ADR-0174: the certificate integration is installed BEFORE the
	// credentials, so that a Comb which was prepared for has its
	// integration in place by the time it needs it - and so that a
	// refusal to install it (an existing token, say) is reported
	// before anything else has been changed.
	//
	// The integration is installed FIRST because it is the thing that
	// can fail on its own terms. The credentials below are the
	// Colony's, and installing them is the more important half.
	if bundle.Integration != nil {
		if err := certintegration.Install(bundle.Integration, managerConfigPath); err != nil {
			return fmt.Errorf("installing the provisioned certificate integration: %w", err)
		}
	}

	// ADR-0187: the joining Comb's OWN raft transport leaf, issued
	// by the member that holds the Colony CA key and sealed to this
	// Comb's certificate, is installed next.
	//
	// It is opened with THIS Comb's private key, so a leaf that was
	// issued for another Comb simply fails to decrypt.
	if bundle.ColonyCA != nil && bundle.ColonyCA.SealedLeaf != "" {
		leaf, err := colonyleaf.Open([]byte(bundle.ColonyCA.SealedLeaf), []byte(servingCert.pem), privateKey,
			requestID, identity.nodeID)
		if err != nil {
			return fmt.Errorf("opening the sealed raft transport leaf: %w", err)
		}
		if err := colonyleafinstall.Install(colonyleafinstall.InstallOptions{
			Leaf:   leaf,
			NodeID: identity.nodeID,
			CADir:  colonycainstall.DefaultCADir,
			Now:    time.Now(),
		}); err != nil {
			return fmt.Errorf("installing the raft transport leaf: %w", err)
		}
		// raftd's own configuration is pointed at the CA and at the
		// leaf, so the transport can be encrypted. It is a separate
		// step because a raftd pointed at material that was never
		// published cannot start.
		if err := colonyleafinstall.PointRaftdAtTheCA(raftdconfig.DefaultPath,
			colonycainstall.DefaultCADir,
			colonyleafinstall.DefaultLeafCertPath,
			colonyleafinstall.DefaultLeafKeyPath); err != nil {
			return fmt.Errorf("pointing raftd at the Colony CA: %w", err)
		}
	}

	if err := installJoinCredentialBundle(bundle, managerConfigPath, frontendconfig.DefaultPath, defaultRestartGuardrailTokenPath); err != nil {
		return err
	}
	for _, service := range []string{"apiary_managerd", "apiary_frontend"} {
		if err := exec.Command("service", service, "restart").Run(); err != nil {
			return fmt.Errorf("credentials were written, but restarting %s failed; restart it manually before relying on Colony forwarding or restart-guardrail confirmations", service)
		}
	}
	return nil
}

// installJoinCredentialBundle writes only this Comb's local credential files.
// Existing API keys are preserved. An existing guardrail token must match the
// Colony token exactly; a mismatch is never repaired by overwriting it.
func installJoinCredentialBundle(bundle joincredential.Bundle, managerConfigPath, frontendConfigPath, tokenPath string) error {
	// Version 1 carries the Colony's peer credentials. Version 2 adds
	// the certificate integration (ADR-0174), for a Comb that was
	// prepared for. Version 3 adds the Colony CA and the joining
	// Comb's own raft transport leaf (ADR-0186, ADR-0187). A version
	// outside those three is refused rather than half-read.
	if bundle.Version != 1 && bundle.Version != 2 && bundle.Version != 3 {
		return fmt.Errorf("credential bundle uses an unsupported version %d", bundle.Version)
	}
	if bundle.RequestID == "" || bundle.NodeID == "" || bundle.PeerAPIKey == "" || bundle.RestartGuardrailToken == "" {
		return errors.New("credential bundle is incomplete")
	}
	nodeManager := &nodeconfig.Manager{Path: managerConfigPath}
	nodeCfg, err := nodeManager.Load()
	if err != nil {
		return fmt.Errorf("reading managerd configuration: %w", err)
	}
	frontManager := &frontendconfig.Manager{Path: frontendConfigPath}
	frontCfg, err := frontManager.Load()
	if err != nil {
		return fmt.Errorf("reading frontend configuration: %w", err)
	}
	if nodeCfg.PeerAPIKey == "" {
		nodeCfg.PeerAPIKey = bundle.PeerAPIKey
	}
	if frontCfg.ManagerAPIKey == "" {
		frontCfg.ManagerAPIKey = bundle.PeerAPIKey
	}
	writeNodeConfig := nodeCfg.PeerAPIKey == bundle.PeerAPIKey
	writeFrontendConfig := frontCfg.ManagerAPIKey == bundle.PeerAPIKey

	tokenExists := false
	if info, err := os.Lstat(tokenPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to use %s because it is not a regular file", tokenPath)
		}
		if info.Mode().Perm() != 0o600 {
			return fmt.Errorf("refusing to use %s with mode %04o; set it to 0600 by hand", tokenPath, info.Mode().Perm())
		}
		current, err := os.ReadFile(tokenPath)
		if err != nil {
			return fmt.Errorf("reading existing restart-guardrail token: %w", err)
		}
		if !bytes.Equal([]byte(strings.TrimSpace(string(current))), []byte(bundle.RestartGuardrailToken)) {
			return errors.New("this Comb already has a different restart-guardrail token; it was left unchanged and no credentials were installed")
		}
		tokenExists = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking restart-guardrail token path: %w", err)
	}

	if writeNodeConfig {
		if err := nodeManager.Save(nodeCfg); err != nil {
			return fmt.Errorf("saving managerd peer credential: %w", err)
		}
	}
	if writeFrontendConfig {
		if err := frontManager.Save(frontCfg); err != nil {
			return fmt.Errorf("saving frontend manager credential: %w", err)
		}
	}
	if !tokenExists {
		if err := writeNewTokenAtomic(tokenPath, bundle.RestartGuardrailToken); err != nil {
			return fmt.Errorf("installing Colony restart-guardrail token: %w", err)
		}
	}
	return nil
}

// writeNewTokenAtomic creates a 0600 token file without replacing an existing
// file. A hard link from a same-directory temporary file gives the final name
// an atomic, no-overwrite publication point on the supported FreeBSD filesystems.
func writeNewTokenAtomic(path, token string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".restart-guardrail-token-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	return nil
}
