# Apiary

A FreeBSD-native virtualization management platform for cluster
management, VM and container orchestration, storage replication, and a
web frontend.

Apiary is built around bhyve-backed virtual machines and jails under one
unified abstraction, with cluster consensus handled by a dedicated raft
agent rather than a bolted-on external dependency.

## Status

Early development.
Architecture and API contracts are being settled before implementation
work begins in earnest.

## Architecture

- **Language:**
  Go, across the entire stack
- **Hypervisor:**
  bhyve, for both VMs and containers
- **Storage:**
  ZFS, replicated node-to-node via HAST
- **Consensus:**
  a dedicated raft agent (HashiCorp's raft library), run as a separate
	process from the management daemon, communicating over a schema-defined
	Unix domain socket protocol
- **External API:**
  RPC-style first, with explicit named operations (CreateVM, MigrateVM, and
	so on); a REST translation layer sits on top afterward for broader client
	compatibility
- **Frontend:**
  a Go backend serving a JSON API, with HTMX handling interactive elements
	server-side

### Terminology

- **physical**:
  HAST-replicated disk bytes and ZFS datasets
- **ephemeral**:
  small JSON-shaped facts, such as cluster membership, VM definitions, and
	node ownership assignments

Physical data stays local per node, already replicated by HAST.
Ephemeral state is what raft actually replicates across the cluster.

## Repository layout

- `cmd/` — entry points for each binary (raftd, managerd, frontend)
- `api/` — protobuf schema definitions, external and internal
- `internal/` — core logic: bhyve, jails, ZFS, HAST, cluster state, raft,
  the REST shim, and the management daemon itself
- `web/` — HTML templates and static assets for the frontend
- `docs/adr/` — architecture decision records

## License

See [LICENSE](LICENSE).
