package joinauth

// The write side of the store: `apiaryctl join-authorize`, the
// operator-facing command that creates one entry.
//
// It is a subcommand of the already-installed root-only apiaryctl
// rather than a new tool, and it needs no checkout, because a Comb is
// not a machine that has one. See cmd/apiaryctl/joinauthorize.go for
// the argument surface and the exit statuses; this file holds the
// decision, so it is testable without a root shell, a real raftd, or a
// filesystem that happens to be laid out the way a Comb's is.

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// PendingRequest is the part of a replicated PendingJoinRequest that
// authorize actually needs. A narrow struct rather than the proto
// message, for two reasons: the decision below must be exercisable
// without raft, and a field this package does not read is a field whose
// absence cannot be mistaken for a change in behaviour later.
type PendingRequest struct {
	RequestID       string
	NodeID          string
	RaftBindAddress string
	Fingerprint     string
	RequestedAtUnix int64
	ExpiresAtUnix   int64
	Status          string
}

// AuthorizeOptions is the whole surface of the operation. There is
// deliberately no field that can name a path other than Path, no field
// that can disable a refusal, and no dry-run: the command's value is
// that it is the one documented way to create an entry, and every knob
// added here is a way to create one that nothing else will accept.
type AuthorizeOptions struct {
	// Path is the store to read and write. Empty means DefaultPath.
	Path string

	// NodeID and Fingerprint are what the operator read off the joining
	// Comb's own page. Both are required, and both are compared exactly
	// (modulo fingerprint case) - there is no wildcard, because an entry
	// that named a Comb without naming its certificate would be
	// authorizing whatever certificate that identity presented next.
	NodeID      string
	Fingerprint string

	// RequestID narrows the match to one specific pending request when
	// the same Comb has more than one outstanding. Empty means "the one
	// that matches", and MORE THAN ONE match is a refusal rather than a
	// choice: two requests for the same identity is a situation an
	// operator should look at, not one to resolve by picking the newest.
	RequestID string

	// TTL is how long the new entry lasts. Zero means DefaultTTL
	// (24 hours, ADR-0147 question 1).
	TTL time.Duration

	// Now defaults to time.Now. A parameter so an expiry can be tested
	// without waiting a day, and so this package never reads a clock
	// behind a caller's back - the same rule the FSM follows for the
	// same reason.
	Now func() time.Time

	// Out receives the human-facing report, including the matched
	// request. It is printed BEFORE the write, not after, so an
	// operator watching a terminal sees the comparison they are being
	// asked to make and can still interrupt before anything is
	// changed.
	Out io.Writer
}

// AuthorizeResult is what was written, for the caller to report on.
type AuthorizeResult struct {
	Entry   Entry
	Matched PendingRequest
	Created bool
}

// Authorize matches the operator's node_id and fingerprint against the
// pending requests this Comb holds, prints the one it matched, and only
// then writes an entry for it.
//
// The order is the point. The ADR asks for the deliberate comparison
// still to happen in front of the operator, and a command that wrote
// first and printed afterwards would have made the comparison a
// formality - the entry would exist whether or not anyone read the
// line. So: match, print, write. A failure anywhere before the write
// leaves the file exactly as it was.
//
// Every refusal names the file, and the refusals are distinct because
// each has a different next step. "No pending request" is the common
// one during an incident - the join window may be closed, the request
// may have expired, or the request may have been made against a
// different member - and it is the one where a wrong guess is most
// likely, so it prints what IS pending rather than only what is not.
func Authorize(opts AuthorizeOptions, pending []PendingRequest) (AuthorizeResult, error) {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	path := opts.Path
	if path == "" {
		path = DefaultPath
	}
	if strings.TrimSpace(opts.NodeID) == "" {
		return AuthorizeResult{}, fmt.Errorf("joinauth: --node-id is required: it is the joining Comb's own raft identity, as shown on that Comb's Machine page")
	}
	if strings.TrimSpace(opts.Fingerprint) == "" {
		return AuthorizeResult{}, fmt.Errorf("joinauth: --fingerprint is required: an entry that names a Comb without naming its certificate would authorize whatever certificate that identity presented next")
	}

	matches := matchPending(pending, opts)
	switch {
	case len(matches) == 0:
		return AuthorizeResult{}, fmt.Errorf("joinauth: no pending join request on this Comb names node_id %q with fingerprint %q%s\n%s",
			opts.NodeID, opts.Fingerprint, requestIDSuffix(opts.RequestID), describePending(pending))
	case len(matches) > 1:
		return AuthorizeResult{}, fmt.Errorf("joinauth: %d pending join requests on this Comb name node_id %q with fingerprint %q (%s); re-run with --request-id to say which one you compared against, rather than having this command pick for you",
			len(matches), opts.NodeID, opts.Fingerprint, joinRequestIDs(matches))
	}
	matched := matches[0]

	// Printed before the store is read, let alone written. This is the
	// comparison the ADR says must happen in front of the operator, and
	// it is the only moment at which the operator and this command are
	// looking at the same thing at the same time.
	fmt.Fprintf(out, "Pending join request this entry will authorize:\n\n%s\n\n", describeRequest(matched))

	// loadForWrite, not Load: this is the one path that is about to
	// create the store if it is not there. See loadForWrite's own doc
	// comment for why a missing file is a refusal for the READER and
	// not for the writer.
	store, err := loadForWrite(path)
	if err != nil {
		return AuthorizeResult{}, err
	}

	// A duplicate is refused rather than written. Two identical entries
	// would both be matchable, which looks harmless and is not: the
	// second would be spendable by a second request for the same
	// identity, which is the single-use property the entry exists to
	// provide. An existing entry is already the authorization; the
	// operator who needs a NEW one has a new fingerprint or a new
	// request, and both of those are visible right here.
	for _, e := range store.Authorizations {
		if e.NodeID == opts.NodeID && NormalizeFingerprint(e.Fingerprint) == NormalizeFingerprint(opts.Fingerprint) {
			if e.ExpiresAtUnix > now().Unix() {
				return AuthorizeResult{}, fmt.Errorf("joinauth: %s already holds entry %q authorizing node_id %q with fingerprint %q, expiring %s - it has not been spent and does not need replacing. If the joining Comb's certificate changed, run this again with the new fingerprint",
					path, e.ID, e.NodeID, e.Fingerprint, time.Unix(e.ExpiresAtUnix, 0).Format(time.RFC3339))
			}
			return AuthorizeResult{}, fmt.Errorf("joinauth: %s already holds entry %q for this exact node_id and fingerprint, but it expired at %s. Replacing it: remove the old entry first if you want the file to keep only live authorizations",
				path, e.ID, time.Unix(e.ExpiresAtUnix, 0).Format(time.RFC3339))
		}
	}

	id, err := NewEntryID()
	if err != nil {
		return AuthorizeResult{}, fmt.Errorf("joinauth: generating an entry id: %w", err)
	}
	entry := Entry{
		ID:            id,
		NodeID:        opts.NodeID,
		Fingerprint:   opts.Fingerprint,
		ExpiresAtUnix: now().Add(ttl).Unix(),
	}
	store.Authorizations = append(store.Authorizations, entry)
	if err := store.Save(path); err != nil {
		return AuthorizeResult{}, err
	}

	fmt.Fprintf(out, "Wrote entry %s to %s: authorizes node_id %q with fingerprint %q until %s.\n",
		entry.ID, path, entry.NodeID, entry.Fingerprint,
		time.Unix(entry.ExpiresAtUnix, 0).Format(time.RFC3339))
	fmt.Fprintf(out, "\nThis entry is single-use and is only read by the Colony's current LEADER.\nIf this Comb is not the leader, create the entry there too: an entry on a\nfollower is an entry no approval will ever consult.\n")
	return AuthorizeResult{Entry: entry, Matched: matched, Created: true}, nil
}

// matchPending returns the pending requests that name this node_id and
// fingerprint, newest first, so a caller reporting "there are N" is
// reporting them in the order an operator would think about them.
func matchPending(pending []PendingRequest, opts AuthorizeOptions) []PendingRequest {
	want := NormalizeFingerprint(opts.Fingerprint)
	var matches []PendingRequest
	for _, p := range pending {
		if opts.RequestID != "" && p.RequestID != opts.RequestID {
			continue
		}
		if p.NodeID != opts.NodeID || NormalizeFingerprint(p.Fingerprint) != want {
			continue
		}
		matches = append(matches, p)
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].RequestedAtUnix > matches[j].RequestedAtUnix })
	return matches
}

func requestIDSuffix(requestID string) string {
	if requestID == "" {
		return ""
	}
	return fmt.Sprintf(" (looking only at request_id %q)", requestID)
}

func joinRequestIDs(reqs []PendingRequest) string {
	ids := make([]string, len(reqs))
	for i, r := range reqs {
		ids[i] = r.RequestID
	}
	return strings.Join(ids, ", ")
}

// describeRequest renders one pending request as the block an operator
// compares by eye against the joining Comb's own screen. Every field
// that identifies the request is on it, because a comparison against
// one field of six is not a comparison.
func describeRequest(p PendingRequest) string {
	fp := p.Fingerprint
	if fp == "" {
		fp = "(no TLS certificate presented by the joining Comb - this entry names no certificate, and will match only another request that also presented none)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  request_id:       %s\n", p.RequestID)
	fmt.Fprintf(&b, "  node_id:          %s\n", p.NodeID)
	fmt.Fprintf(&b, "  raft address:     %s\n", p.RaftBindAddress)
	fmt.Fprintf(&b, "  fingerprint:      %s\n", fp)
	fmt.Fprintf(&b, "  status:           %s\n", p.Status)
	fmt.Fprintf(&b, "  requested at:     %s\n", time.Unix(p.RequestedAtUnix, 0).Format(time.RFC3339))
	fmt.Fprintf(&b, "  expires at:       %s\n", time.Unix(p.ExpiresAtUnix, 0).Format(time.RFC3339))
	return b.String()
}

// describePending lists what IS pending, so a refusal about a request
// that is not there is diagnosable from the refusal itself. An empty
// list says so in words rather than printing nothing, because "no
// pending requests" and "your grep matched nothing" are different
// answers.
func describePending(pending []PendingRequest) string {
	if len(pending) == 0 {
		return "\n  There are no pending join requests on this Comb at all. Either the\n  joining Comb has not requested one yet, or the request was made\n  against a different Colony member, or it has already been resolved."
	}
	var b strings.Builder
	b.WriteString("\n  Pending join requests on this Comb:\n")
	for _, p := range pending {
		fmt.Fprintf(&b, "    %s  node_id %q  fingerprint %s  status %s\n",
			p.RequestID, p.NodeID, orNone(p.Fingerprint), p.Status)
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
