# ADR-0001: raftd process split, UDS protocol, and proto tooling

## Status

Accepted

## Context

This is the first real code in Apiary: a single-node `raftd` that
bootstraps a one-node raft cluster and exposes it over the internal
protocol that `managerd` will eventually consume. Several foundational,
hard-to-reverse choices had to be made to write this slice at all, and
none were previously recorded anywhere. This ADR captures them before
they're forgotten or silently relied upon.

## Decisions

### raftd is a separate OS process, not an in-process library

Per the project's stated architecture, cluster consensus is owned by a
dedicated `raftd` process rather than an embedded raft/etcd/Consul
dependency inside `managerd`. This slice implements that split literally:
`cmd/raftd` is its own binary with its own lifecycle, and `internal/raft`
knows nothing about `managerd`.

### The internal protocol is gRPC over a Unix domain socket, not hand-rolled framing

`managerd` and `raftd` talk over a Unix domain socket. We use gRPC over
that socket (`net.Listen("unix", path)` on the server, `grpc.NewClient
("unix://"+path, ...)` on the client) rather than a bespoke
length-prefixed wire format.

Rationale: the only property being exploited by "Unix socket instead of
TCP" is that it's local-machine-only — there's no different framing
requirement. gRPC's HTTP/2 framing works fine over a Unix socket, and
using it gets typed request/response messages, deadline/context
propagation, and status codes for free, straight from the same proto
schema. A hand-rolled protocol would have to reinvent all of that for no
benefit.

### Proto schemas are compiled with buf, not raw protoc

`buf` (via `buf.yaml` + `buf.gen.yaml`) drives codegen instead of a
system-installed `protoc` binary plus manually-versioned
`protoc-gen-go`/`protoc-gen-go-grpc` plugins. There was no pre-existing
protoc convention in this repo to preserve, and buf's lockfile-pinned
tooling is more reproducible across contributor machines and CI than
requiring everyone to install a matching protoc.

### Generated code is committed alongside its .proto source

`api/internalpb/raftd.pb.go` and `raftd_grpc.pb.go` live in the same
directory as `raftd.proto`, in the same Go package, rather than under a
separate `gen/` tree. This matches how `go_package` naturally resolves
and avoids keeping two directory trees in sync. Generated files are
committed so `go build` never requires buf or protoc plugins to be
installed on a build host (including target FreeBSD machines).

### `api/internal` was renamed to `api/internalpb`

The repository layout as originally documented named this directory
`api/internal`. Go's internal-package visibility rule restricts importers
of any package under a directory literally named `internal` to code
rooted at that directory's parent — here, that would have meant only code
under `api/` could import it. That's the opposite of what's needed:
`raftd` (`cmd/raftd`, `internal/raft`) and eventually `managerd` are
exactly the intended consumers, and neither lives under `api/`. Renaming
to `api/internalpb` (matching the already-chosen Go package name
`internalpb`) resolves this while keeping the directory's purpose
unambiguous. `CLAUDE.md` and `README.md` have been updated to match.

### raft-boltdb/v2 for log/stable storage

`github.com/hashicorp/raft-boltdb/v2` is used for both the `LogStore` and
`StableStore` (a single BoltDB file, `raft.db`, in the node's data
directory), plus `raft.NewFileSnapshotStore` for snapshots. The original
`raft-boltdb` (v1) is in maintenance mode; `/v2` is what HashiCorp
currently points users toward, and it's what backs durability across
restarts — the entire point of choosing a real store over in-memory ones
for anything beyond a throwaway test.

### A real TCP transport on loopback, not `InmemTransport`, even for v1's single node

This slice only supports single-node bootstrap — no peer join/remove yet.
Even so, `raft.NewTCPTransport` binds a real loopback address
(configurable via `-raft-bind`, default `127.0.0.1:17600`) rather than
using raft's `InmemTransport` (which is intended for same-process tests,
not for a real node that may one day gain peers). This means the
multi-node clustering slice can add peer-join logic against the same
transport without a rewrite.

## Consequences

- Any future proto package under `api/` that needs to be importable from
  outside `api/` must avoid naming its directory `internal` — this is now
  a known constraint, not a surprise to rediscover.
- `buf` must be installed for anyone regenerating proto stubs (`buf
  generate`), though not for building or running the project day-to-day
  since generated code is committed.
- `raftd`'s data directory holds a real file-locked BoltDB database,
  meaning only one `raftd` process may hold a given `-data-dir` open at a
  time; a second instance pointed at the same directory will block
  waiting for the flock rather than starting concurrently. This is
  intentional (the same directory is one node's exclusive state) and is
  the same failure mode a stale, unclosed handle produced during
  development — see the raftd shutdown fix that closes the BoltDB store
  on `Node.Shutdown`.
