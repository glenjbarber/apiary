// Package peerca materializes /usr/local/etc/apiary/peer-ca.pem, the
// trust anchor ADR-0147 Part 4's replicated peer trust store exists to
// produce.
//
// Why the file exists at all, given the store already holds the
// certificates: the trust anchors consumers need are not a proto
// message. managerd, the frontend, and cmd/raftd's replacement-
// confirmation path all want an x509.CertPool, and all three already
// get one from LoadPeerCAPool(peer_tls_ca). What was missing is a
// writer, so the ADR retires the MANUAL distribution step in
// docs/add-node-to-colony.md - copying a CA PEM between hosts by hand -
// rather than retiring peer_tls_ca as a concept. An operator who sets
// a real path still wins; this file is what covers the rest.
//
// Three properties are load-bearing, and each is why this is a package
// of its own rather than four lines in cmd/managerd:
//
//   - DETERMINISTIC BYTES. Every Comb writes this file from its own
//     replicated copy, sorted by node ID, so the bytes are identical
//     on every host and an operator can diff two Combs. A writer that
//     emitted map order would produce a different file on every write
//     and make that diff useless.
//   - IT IS A DERIVED CACHE. Delete it and the next managerd start
//     recreates it correctly, so it never has to be in a backup and
//     never has to be restored. That is why this package refuses to
//     treat a missing file as an error.
//   - ONE WRITER. managerd, and only managerd, on this Comb. A second
//     writer would be a second source of truth about trust, which is
//     the thing the replicated store was introduced to end.
//
// It is a leaf bundle, not a CA. The entries are self-signed serving
// certificates, and the file is a pin list: crypto/x509 accepts a
// self-signed leaf in a RootCAs pool and will chain to it, which is
// precisely the behaviour wanted here. The names are NOT CN/SAN
// fields - a file that renames certificates to something that parses
// would make the file unreadable as the record of what is trusted,
// which is the second thing an operator opens it for.
package peerca

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// DefaultPath is the derived file's location. Stated once, here, next
// to the writer: the ADR, the config default, and the installer's
// reports all have to name the same path, and three copies of a
// string literal is how they stop naming the same path.
const DefaultPath = "/usr/local/etc/apiary/peer-ca.pem"

// Render produces the file's exact bytes for a set of pins.
//
// Sorted by node ID regardless of the order it is handed, rather than
// trusting the caller to have sorted already. ListTrustedPeersLocal
// does sort, but this function is what makes the bytes deterministic,
// and a property that lives only in one caller's discipline is a
// property that stops holding the day a second caller appears.
//
// An entry whose PEM is empty is a REFUSAL, not a skip. Skipping it
// would write a file that quietly does not contain a pin the operator
// believes is in the store, and the failure would surface later as a
// handshake against a host nobody can explain - which is the exact
// diagnosis the derived file exists to make possible.
func Render(peers []*internalpb.TrustedPeer) ([]byte, error) {
	ordered := make([]*internalpb.TrustedPeer, len(peers))
	copy(ordered, peers)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].GetNodeId() < ordered[j].GetNodeId()
	})

	var buf bytes.Buffer
	for _, peer := range ordered {
		pem := peer.GetCertPem()
		if pem == "" {
			return nil, fmt.Errorf("peerca: the pin for node_id %q carries no certificate; the trust store and this file must not disagree", peer.GetNodeId())
		}
		// The PEM is stored already-normalized by the FSM (it round
		// trips through hostcert.Evaluate before it is stored), so it
		// is emitted as stored rather than re-encoded. Re-encoding
		// would be a second definition of "the same certificate" and
		// could differ from the bytes the pin's fingerprint was
		// derived from, which is the one thing this file must never
		// do.
		buf.WriteString(pem)
		if !bytes.HasSuffix([]byte(pem), []byte("\n")) {
			buf.WriteString("\n")
		}
	}
	return buf.Bytes(), nil
}

// Write renders peers and writes the file atomically, 0600, root-owned.
//
// 0600 and not 0644 - which is what the installer's own certificate
// file is - because this file is not a public certificate. It is a
// list of which peers this Colony has decided to trust, and every
// entry in it is one an operator went to the trouble of comparing by
// hand. A world-readable trust list is a target.
func Write(path string, peers []*internalpb.TrustedPeer) error {
	body, err := Render(peers)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("peerca: creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".peer-ca-*.tmp")
	if err != nil {
		return fmt.Errorf("peerca: creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // a no-op once renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("peerca: writing temp file for %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("peerca: syncing temp file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("peerca: closing temp file for %s: %w", path, err)
	}
	// chmod before rename, never after: between the two the file exists
	// under its final name, and a window in which it is readable by
	// anyone is exactly the window this mode exists to close.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("peerca: setting permissions on %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("peerca: renaming into place at %s: %w", path, err)
	}
	return nil
}
