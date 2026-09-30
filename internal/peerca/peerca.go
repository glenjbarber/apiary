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
//   - IT IS A DERIVED CACHE, FOR A TRUST STORE THAT HAS PINS. Delete
//     it and the next managerd start recreates it correctly, so it
//     never has to be in a backup and never has to be restored. That
//     is why this package refuses to treat a missing file as an error.
//
// The second half of that property is the one that was wrong, and it
// cost an outage. "Recreates it correctly" holds only when the
// replicated store is non-empty. A Colony inherited from a build that
// predates the store has no pins in it, and an earlier version of
// Render turned that absence of pins into a zero-byte file and renamed
// it over whatever was there - destroying a hand-distributed bundle
// that every member of the Colony was using, and leaving behind a
// file that managerd's own reader then refuses to load, which is a
// crash loop rather than an outage. So an empty trust store is a
// REFUSAL (ErrEmptyTrustStore) rather than a publishable truth: the
// writer leaves the file on disk untouched and reports why. A derived
// cache must never be the thing that destroys the only copy of the
// operator's trust.
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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// ErrEmptyTrustStore is what Render and Write report when the
// replicated peer trust store holds no pins at all.
//
// It is exported and sentinel-checkable because the caller needs to
// tell this apart from every other failure: an unreadable file or a
// corrupt pin is a fault to investigate, whereas an empty store is a
// known, documented state - a fresh install, or a Colony whose
// members were admitted before the store existed and were never
// backfilled - that needs a deliberate operator action (pin the
// members' certificates) rather than a repair. Collapsing the two
// into one error is what would leave an operator reading a crash loop
// with no idea that the file is empty on purpose.
//
// It is deliberately NOT a success. A Colony that trusts no peers and
// a Colony that has not yet recorded which peers it trusts are
// different states, and reporting the second as the first is how peer
// TLS ends up encrypted-but-unverified - a worse outcome to discover
// during a failure than having no encryption at all.
var ErrEmptyTrustStore = errors.New("peerca: the peer trust store is empty; an empty bundle is not a trust set, so it is not published over a file that has one - pin the members' certificates to backfill this Colony")

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
//
// So is an entry-count of zero, for the same reason and with a worse
// consequence. Skipping every entry would write an empty file, and an
// empty file replaces a good one atomically; the reader then refuses
// to start. The one thing a derived cache must never do is remove the
// trust it was derived from.
func Render(peers []*internalpb.TrustedPeer) ([]byte, error) {
	if len(peers) == 0 {
		return nil, ErrEmptyTrustStore
	}
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
//
// Write NEVER damages an existing file. Every failure path returns
// before the rename, so a file already on disk is left byte-for-byte
// as it was - including the empty-store refusal, which is the whole
// reason Render rejects a zero-length trust store. A writer for a
// trust anchor has to be safe to run on a tick against a Colony whose
// store it does not yet understand, and the only way to be safe on a
// tick is to refuse rather than to clobber. The temporary file is
// created only after the render has succeeded, so a refusal does not
// even leave a stray .peer-ca-*.tmp behind.
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
