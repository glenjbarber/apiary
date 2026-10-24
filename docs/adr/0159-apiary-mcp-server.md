# ADR-0159: Apiary read-only MCP server and guarded actions

## Status

Proposed. This record documents the current implementation and proposes the product and security boundary for issue #26. Deployment and client interoperability are not verified.

Supersedes: None
Superseded by: None
Tracking issue: #26

## Context

`cmd/apiary-mcp` already provides a narrow read-only MCP server. It exposes `list_cells`, `get_cell`, and `get_cluster_health`, backed by the read RPCs ListVMs, ListJails, GetVM, GetJail, and ClusterHealth. It supports stdio and Streamable HTTP. HTTP currently binds to loopback, requires a bearer token, and serves `/mcp`. The server registers tools directly; there is no dynamic tool-extension mechanism. The connector package is a packaged client integration, not an in-process Apiary plugin framework.

The MCP server is one adapter over ManagerService, not a new backend API or an MCP host. Its current implementation is a useful starting point, but the boundary for basic read-only external-client access remains unresolved. The separate orcli repository is the MCP host/client. Its local stdio behavior is preserved; its remote Streamable HTTP client work is tracked by orcli#62.

## Decision

The first supported tool set is the implemented three-tool read-only set: `list_cells`, `get_cell`, and `get_cluster_health`. These map respectively to ListVMs plus ListJails, GetVM or GetJail, and ClusterHealth. The server allowlist and ManagerService authorization are both required. The underlying read RPCs require Viewer authorization; a missing authorization-map entry must continue to fail closed. Tool responses remain bounded summaries and do not expose raw diagnostic detail or credentials.

Basic read-only MCP access is not an approved paid-only feature by this ADR. Whether it belongs in the core product or a paid plugin remains open for the owner. Until decided, keep the server's documented read-only implementation available as a distinct adapter, and do not introduce a license check or claim that external clients are included in a paid tier. Read-only access never implies permission to mutate Apiary.

The deployment target is a selected control host that can reach managerd over its configured authenticated TLS connection. For a local MCP client, configure stdio on that host with the Apiary MCP executable. For a remote client, use Streamable HTTP at `/mcp` behind an operator-managed HTTPS reverse proxy or private tunnel; do not bind the MCP process directly to a non-loopback interface. Configuration uses the existing environment surface: `APIARY_MANAGER_ADDR`, `APIARY_API_KEY`, optional `APIARY_MANAGER_CA` and `APIARY_MANAGER_SERVER_NAME`, `APIARY_MCP_TRANSPORT`, `APIARY_MCP_LISTEN`, and `APIARY_MCP_TOKEN` for HTTP mode. The API key and bearer token are secrets and must be supplied by the host's secret-management facility, not committed to a config file or logged.

Current authentication modes are managerd's API-key authorization for backend calls, plus local process access for stdio; HTTP adds the loopback bearer token. Remote operation requires HTTPS from the client to the reverse proxy or private tunnel and the existing authenticated TLS connection from the server to managerd. The current HTTP handler is stateless and does not retain MCP session state; stdio state is limited to the local process. Remote user identity propagation, OAuth, per-user MCP sessions, and token exchange are not specified by the current implementation and remain open. Do not treat the shared bearer token as a user identity or as a substitute for ManagerService authorization.

No write tool is supported. A later guarded-write phase requires an explicit owner decision and a separate design for each operation. Every write must map to a ManagerService RPC with an explicit minimum role, preserve server-side authorization, bind approval to the specific action and parameters, require human approval immediately before execution, and produce an audit record with caller identity, action, authorization result, approval, and outcome while excluding secrets. Tools absent from the explicit allowlist remain unavailable. No generic shell, arbitrary RPC, REST proxy, or dynamic tool registration is permitted.

Orcli owns client transport and interaction behavior. Apiary owns the MCP server, tool definitions and schemas, backend RPC mapping, authorization, output bounds, and server-side auditing. Delivery follows the agreed order: orcli client foundation first, including local stdio preservation and remote transport follow-up orcli#62; Apiary read-only server second; guarded actions only after their separate policy and authorization design. Neither repository should implement the other's connection or operation behavior.

## Consequences

- The present read-only server is recorded as the baseline; no claim is made that deployment or client interoperability has been verified.
- Apiary remains a resource server. Orcli remains the client/host, with independent work in the orcli repository.
- The basic read-only external-client product boundary, remote user identity model, session behavior, and any licensing decision remain open for the owner.
- Guarded writes stay out of scope until per-operation role mapping, explicit user approval, and auditable outcomes are specified and implemented.
- ADR numbers 0157, 0158 and 0159 are verified free in the current origin/main tree, but central ADR number allocation still requires owner confirmation.
