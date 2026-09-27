package raft

import (
	"fmt"
	"net"
	"strconv"
)

// RequestJoinColony is deliberately unauthenticated (a joining Comb has no
// Colony credential yet), and every accepted call becomes a replicated Raft
// record kept in every voter's state and snapshots. These limits bound what
// an unauthenticated caller can make the whole Colony store.
const (
	// MaxJoinRequests is the most join-request records the FSM will hold, in
	// any status. Expired records past the retention window are evicted first,
	// so this only rejects when that many are genuinely recent.
	MaxJoinRequests = 100

	// MaxActionableJoinRequests is the most currently-pending (not expired)
	// requests the RPC layer accepts before refusing to write more to the
	// Raft log at all. It sits well under MaxJoinRequests so a flood is
	// refused before it reaches the log, not only when the FSM applies it.
	MaxActionableJoinRequests = 25

	// joinRequestRetentionAfterExpiry is how long an expired record is kept
	// (so a joining Comb polling its own request sees a real expiry instead of
	// "not found") before a later create may evict it.
	joinRequestRetentionAfterExpiry int64 = 3600

	maxJoinNodeIDLen      = 255
	maxJoinBindAddressLen = 255
	maxJoinFingerprintLen = 256
	maxJoinCodeLen        = 32
)

func validJoinNodeID(s string) bool {
	if s == "" || len(s) > maxJoinNodeIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
		default:
			return false
		}
	}
	return true
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// ValidateJoinRequestFields checks the caller-supplied strings of a join
// request for shape and length. It is used both by the RPC layer (so a bad
// request never reaches the Raft log) and by the FSM (so every replica reaches
// the identical decision).
func ValidateJoinRequestFields(nodeID, raftBindAddress, tlsFingerprint string) error {
	if !validJoinNodeID(nodeID) {
		return fmt.Errorf("node_id must be 1 to %d characters of letters, digits, '.', '-' or '_'", maxJoinNodeIDLen)
	}
	if len(raftBindAddress) == 0 || len(raftBindAddress) > maxJoinBindAddressLen || !printableASCII(raftBindAddress) {
		return fmt.Errorf("raft_bind_address must be a host:port of at most %d printable characters", maxJoinBindAddressLen)
	}
	host, port, err := net.SplitHostPort(raftBindAddress)
	if err != nil || host == "" {
		return fmt.Errorf("raft_bind_address %q is not a valid host:port", raftBindAddress)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("raft_bind_address %q has an invalid port", raftBindAddress)
	}
	if len(tlsFingerprint) > maxJoinFingerprintLen || (tlsFingerprint != "" && !printableASCII(tlsFingerprint)) {
		return fmt.Errorf("tls_cert_fingerprint must be at most %d printable characters", maxJoinFingerprintLen)
	}
	return nil
}
