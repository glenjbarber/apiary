package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/joinauth"
	"github.com/glenjbarber/apiary/internal/localraft"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// runJoinAuthorize is `apiaryctl join-authorize`, the operator-facing
// half of ADR-0147 Part 3: the one documented way to create an entry in
// the root-owned authorization store that admits a Comb to this Colony.
//
// It reads this Comb's own raftd socket rather than dialing managerd,
// and that is not a shortcut. managerd's ListJoinRequests is Admin-gated
// on a Colony API key, and root on a Comb does not have one - the
// operator running this command is root precisely because they are NOT
// an Admin of the Colony, and asking them for an API key would be asking
// for the credential whose independence from root is the entire point.
// The local raftd socket is mode 0660 inside a 0700 directory, so root
// can read it and nothing else can, which is the same authority the
// authorization store itself relies on.
//
// It needs no Colony to be reachable and no network at all: the pending
// requests it compares against are this Comb's own replicated copies, and
// the entry it writes is read only by whichever Comb is the leader. The
// command therefore prints an explicit warning when this Comb is not the
// leader, because an entry written on a follower is an entry no approval
// will ever consult.
func runJoinAuthorize(args []string) int {
	fs := flag.NewFlagSet("apiaryctl join-authorize", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		nodeID         string
		fingerprint    string
		requestID      string
		authorizations string
		raftdConfig    string
		expiresIn      time.Duration
	)
	fs.StringVar(&nodeID, "node-id", "", "the joining Comb's raft identity, exactly as its own Machine page shows it (required)")
	fs.StringVar(&fingerprint, "fingerprint", "", "that Comb's managerd TLS certificate fingerprint, exactly as its own Machine page shows it (required)")
	fs.StringVar(&requestID, "request-id", "", "narrow the match to one pending request, when the same Comb has more than one outstanding")
	fs.StringVar(&authorizations, "authorizations", "", "path to the authorization store; defaults to "+joinauth.DefaultPath)
	fs.StringVar(&raftdConfig, "raftd-config", "", "path to raftd.json, to find the local raftd socket; defaults to "+raftdconfig.DefaultPath)
	fs.DurationVar(&expiresIn, "expires-in", joinauth.DefaultTTL, "how long the new entry lasts; entries are single-use, so this is a ceiling on an operator forgetting one")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: apiaryctl join-authorize --node-id ID --fingerprint FP [--request-id ID]

Creates ONE single-use entry in this Colony's root-owned authorization
store, authorizing a named Comb to join. It prints the pending join
request it matched against BEFORE it writes anything, because the
comparison between the joining Comb's own screen and that request is the
step no automation can do for you.

Entries are single-use, they expire (24 hours by default), and they are
read ONLY by the Colony's current leader. If this Comb is not the
leader, run this on the leader as well - an entry on a follower is an
entry no approval will ever consult.

Root only, and needs no checkout: this is the installed binary at
/usr/local/libexec/apiary/apiaryctl.

Exit status is 0 only when an entry was written. Every refusal exits
non-zero and writes nothing.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl join-authorize: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if nodeID == "" || fingerprint == "" {
		fmt.Fprint(os.Stderr, "apiaryctl join-authorize: --node-id and --fingerprint are both required.\n\nRead both off the JOINING Comb's own Machine page. They are not on this\nComb's page, and they are not guessable: an entry that named a Comb\nwithout naming its certificate would authorize whatever certificate that\nidentity presented next.\n")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprint(os.Stderr, `apiaryctl join-authorize: must run as root.

The authorization store is root-owned and mode 0600, and that ownership
IS the security property: it is the one thing an Admin of this Colony
cannot do. Writing the store as anyone else would not create an
authorization, it would create a file nobody can trust.

If you are trying to approve the join without authorizing it, the
Colony will refuse the approval and name this command.
`)
		return 1
	}

	conn, err := localraft.Dial(raftdConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-authorize: %v\n", err)
		return 1
	}
	defer conn.Close()

	pending, isLeader, err := listPendingJoinRequests(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, `apiaryctl join-authorize: could not read this Comb's pending join requests: %v

An entry is only worth creating against a request this Colony actually
holds, and this command will not write one blind: an entry nobody
compares against is an authorization nobody gave. Check that apiary_raftd
is running and that raftd.json names the right socket.
`, err)
		return 1
	}

	if _, err := joinauth.Authorize(joinauth.AuthorizeOptions{
		Path:        authorizations,
		NodeID:      nodeID,
		Fingerprint: fingerprint,
		RequestID:   requestID,
		TTL:         expiresIn,
		Out:         os.Stdout,
	}, pending); err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-authorize: %v\n", err)
		return 1
	}

	if !isLeader {
		fmt.Fprintf(os.Stderr, `
apiaryctl join-authorize: this Comb is NOT the Colony's leader.

The entry above was written here and is correct, but only the leader
reads the authorization store - an entry on a follower is one no
approval will ever consult. Run the same command on the leader before
expecting the join to be approvable.
`)
		return 1
	}
	return 0
}

// localRaftdSocket, localRaftdToken and the dial itself moved to
// internal/localraft, which apiaryctl force-restart now uses too. Two
// copies of "read raftd.json, dial that socket, present that token" is
// two places for the two commands to disagree about which socket this
// Comb has, and a disagreement there is not a crash - it is the wrong
// Colony's answer, read as confidently as the right one. The socket and
// the token now come out of the same config load for both commands,
// which is one more thing they can no longer get out of step.
//
// listPendingJoinRequests reads this Comb's own pending join requests,
// and whether this Comb is currently the leader.
//
// The connection is built by internal/localraft rather than here for
// one reason worth stating: apiaryctl is a small root shell tool, and
// dialing raftd by hand rather than through internal/manager is what
// keeps raft, bbolt and the cluster packages out of a binary an
// operator runs during an incident. cmd/raftd's own startup hook builds
// its connection to managerd the same way, for the same reason.
//
// ListPendingJoinRequestsLocal rather than the leader-only ListVMs-style
// read is deliberate: a follower can answer, and an entry created on a
// follower is still the right entry for the leader once it is copied -
// what the command reports is which Comb has to be told separately, not
// a reason to refuse to write.
func listPendingJoinRequests(conn *grpc.ClientConn) (pending []joinauth.PendingRequest, isLeader bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), localraft.DefaultTimeout)
	defer cancel()
	client := internalpb.NewRaftInternalClient(conn)

	status, err := client.Status(ctx, &internalpb.StatusRequest{})
	if err != nil {
		return nil, false, fmt.Errorf("asking this Comb's raftd for its status: %w", err)
	}
	list, err := client.ListPendingJoinRequestsLocal(ctx, &internalpb.ListPendingJoinRequestsRequest{})
	if err != nil {
		return nil, false, fmt.Errorf("asking this Comb's raftd for its pending join requests: %w", err)
	}
	for _, r := range list.GetRequests() {
		pending = append(pending, joinauth.PendingRequest{
			RequestID:       r.GetRequestId(),
			NodeID:          r.GetNodeId(),
			RaftBindAddress: r.GetRaftBindAddress(),
			Fingerprint:     r.GetTlsCertFingerprint(),
			RequestedAtUnix: r.GetRequestedAtUnix(),
			ExpiresAtUnix:   r.GetExpiresAtUnix(),
			Status:          r.GetStatus().String(),
		})
	}
	return pending, status.GetIsLeader(), nil
}
