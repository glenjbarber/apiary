# ADR-0112: Shared common.json config file

## Status

Accepted

## Context

A single Comb runs up to four independent daemons, each with its own
config file under /usr/local/etc/apiary (ADR-0100):
managerd.json (internal/nodeconfig), frontend.json
(internal/frontendconfig), restshimd.json (internal/restshimdconfig),
and raftd.json (internal/raftdconfig). Each package has its own
Config struct and Manager.Load, with no cross-imports and no
shared/include mechanism.

Two of the four already carry the same logical value under the same
name: node_id (internal/nodeconfig.Config.NodeID and
internal/raftdconfig.Config.NodeID) both identify this Comb, and on a
real deployment they are set to the same string. Today that means
typing the same node_id into two separate files by hand, with no
enforcement that they match - a config-file source of the exact kind
of raft-identity mismatch these packages already take care to guard
elsewhere. The project owner asked for a shared common.json so
identical values do not need to be retyped across the four files.

TLS CA scope: frontend.json has both peer_tls_ca (trusted when
frontend dials another Comb's managerd for cluster overview stats,
ADR-0093) and manager_tls_ca (trusted when frontend dials its own
local managerd); restshimd.json has its own manager_tls_ca for the
same local-managerd relationship; raftd.json has raft_tls_ca (trusted
for the raft transport, ADR-0078); nodeconfig has peer_tls_ca as well
(for managerd-to-managerd dialing). These are four different trust
relationships (peer-to-peer, frontend/restshim-to-local-managerd, and
the raft transport), each already a separate field because they can
legitimately be different CA files - a real deployment could terminate
raft TLS off a different CA than its peer-management TLS, or use no
TLS at all for one and not the other. Collapsing them into one shared
tls_ca field would either force every daemon on a Comb to consult the
same CA for meaningfully different purposes, or reintroduce per-field
overrides that make the "one shared field" idea pointless. There is
also no field with the same name already duplicated across two of the
four files the way node_id is - each existing *_tls_ca field is
distinct today. TLS CA is therefore out of scope for v1; only NodeID
(a genuine same-name duplicate today) and Hostname (mentioned by the
project owner as a canonical example of a shared value, reserved for
a daemon that wants it without deriving it from os.Hostname() itself)
go into common.json for now.

## Decision

New package internal/commonconfig mirrors the existing four packages'
own conventions exactly: a Manager struct with a Path field
(defaulting to DefaultPath = /usr/local/etc/apiary/common.json), and
a Load() (Config, error) method where a missing file returns a
zero-value Config with no error, and a malformed file returns a real
parse error. Config has two fields: NodeID and Hostname, both
`omitempty` strings.

internal/nodeconfig.Manager.Load and internal/raftdconfig.Manager.Load
- the two packages with their own node_id field today - now load
common.json from the same directory as their own config file (via
filepath.Dir on the service file's own path, so a non-default -path
flag still finds its sibling common.json) before parsing their own
JSON. The merge rule is a plain two-file merge in Go, not a new
config-file "include" syntax: after the service file is parsed, an
empty NodeID is filled in from common.json's NodeID; a NodeID already
set in the service file itself always wins and common.json's value is
never consulted. A host with no common.json behaves identically to
today - loadCommonConfig treats os.IsNotExist as a zero-value Config,
so applyCommonConfig has nothing to fill in.

internal/frontendconfig and internal/restshimdconfig are not wired up
in this change. Neither package has a node_id or hostname field
today - there is nothing on their Config structs for common.json to
fill in yet - so adding the plumbing now would be dead code with no
observable effect. Wiring them in is a small, mechanical follow-up
(the same loadCommonConfig/applyCommonConfig pattern used in
nodeconfig and raftdconfig) whenever either package gains a field
that actually overlaps with commonconfig.Config.

This is additive and non-breaking: an existing managerd.json or
raftd.json that already sets node_id keeps behaving exactly as it
does today (the service file wins), and a host with no common.json
at all is unaffected, both covered by new tests.

## Consequences

- An operator can put node_id once in
  /usr/local/etc/apiary/common.json instead of duplicating it into
  both managerd.json and raftd.json, removing one way for the two to
  silently drift apart.
- Hostname exists in commonconfig.Config today with no consumer -
  reserved for the first daemon config that wants it, per the project
  owner's own example, rather than left out only to be re-litigated
  later.
- TLS CA unification is explicitly deferred; if a future need arises
  to share a CA file across daemons on the same Comb, it should be
  evaluated on its own, since (per the Context section above) the
  existing per-relationship CA fields are not obviously safe to
  collapse into one.
- internal/frontendconfig and internal/restshimdconfig do not yet
  consult common.json - see Decision above.

## Verification

- New internal/commonconfig/manager_test.go covers: a missing
  common.json loads as a zero-value Config with no error; a populated
  file parses both fields; a malformed file is a real load error.
- internal/nodeconfig/manager_test.go and
  internal/raftdconfig/manager_test.go each gained four new cases:
  common.json fills in node_id when the service file leaves it unset;
  a node_id set in the service file wins over common.json; a missing
  common.json is unaffected (identical to pre-ADR-0112 behavior); a
  malformed common.json is a real load error.
- `go build ./...`, `go vet ./...`, `gofmt -l .`, and the full
  `go test ./...` all pass.
