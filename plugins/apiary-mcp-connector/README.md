# Apiary MCP Connector

This plugin provides `list_cells`, `get_cell`, and `get_cluster_health` through
the `apiary-mcp` binary built from `cmd/apiary-mcp`. The connector calls only
five read RPCs on Apiary's ManagerService (`ListVMs`, `ListJails`, `GetVM`,
`GetJail`, `ClusterHealth`) and returns selected fields. It has no shell,
console, or mutation tool.

## Requirements

- Go 1.27 or later, to build the binary.
- Network reachability from the build host to an Apiary ManagerService
  endpoint.
- A dedicated Apiary API key with the Viewer role. The connector sends the key
  on every call and managerd decides what it may read, so a key with a higher
  role would work but is not the intended configuration.
- The CA certificate and DNS name needed to validate the ManagerService TLS
  certificate.

## Build

From the Apiary repository root:

    go build -o apiary-mcp ./cmd/apiary-mcp

Put the resulting binary on the MCP host's `PATH` as `apiary-mcp`. The plugin
does not compile code at launch.

## Configure the plugin host

Set these environment variables for the process that launches MCP plugins:

| Variable | Purpose |
| --- | --- |
| `APIARY_MANAGER_ADDR` | ManagerService `host:port` |
| `APIARY_API_KEY` | Dedicated Viewer API key; never put it in this plugin package |
| `APIARY_MANAGER_CA` | Optional PEM CA bundle when the system trust store is insufficient |
| `APIARY_MANAGER_SERVER_NAME` | Optional TLS DNS name when it differs from the address host |

The connector requires TLS and validates the server certificate. It never
falls back to plaintext. Each tool call has a 10-second timeout.

Errors name the gRPC status code and not the backend's message. A denied call
reports that the key must have the Viewer role. Each tool call writes one audit
line to stderr with the tool name, outcome, and duration. Arguments, results,
and the key are not logged.

## Remote HTTP transport

The binary also serves Streamable HTTP with `-transport http`. It binds only to
loopback, requires `APIARY_MCP_TOKEN` (at least 32 characters) as a bearer
token, and must sit behind HTTPS and a private tunnel or authenticated reverse
proxy. This change does not verify any hosted client connecting to it.

## Tests

    go test -race ./cmd/apiary-mcp/

The tests run the real MCP server against a fake ManagerService client. They
check that the tool surface is read-only, that only read RPCs are called, that
denials and backend errors are sanitized, and that the HTTP bearer guard and
configuration rules hold.

## Disable

Remove the plugin from the MCP host and revoke its dedicated Viewer API key in
Apiary. The connector does not keep credentials on disk.
