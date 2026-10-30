# Apiary MCP Connector

A read-only view of an Apiary cluster, as a plugin package that works for any
conformant client.

It provides `list_cells`, `get_cell` and `get_cluster_health` through the
`apiary-mcp` binary built from `cmd/apiary-mcp`. The connector calls only five
read RPCs on Apiary's ManagerService (`ListVMs`, `ListJails`, `GetVM`,
`GetJail`, `ClusterHealth`) and returns selected fields. It has no shell,
console, or mutation tool.

## The package

```
apiary-mcp-connector/
├── plugin.json              Agent Plugins 1.0.0 manifest
├── mcp.json                 Agent Plugins MCP configuration
├── .claude-plugin/
│   └── plugin.json          Claude Code manifest
├── .mcp.json                Claude Code MCP configuration
├── bin/
│   └── apiary-mcp           the connector, built by `make plugin-package`
└── skills/
    └── apiary-state/
        └── SKILL.md         reporting discipline for the model using the tools
```

The package satisfies **Agent Plugins 1.0.0** and the **Claude Code plugin**
format at the same time, so one package serves every client rather than one
package per client. `scripts/check-plugin-package.sh` checks it against both
specifications.

The two manifests differ because the specifications differ: Agent Plugins
puts the manifest at the plugin root, Claude Code puts it in
`.claude-plugin/`. The two MCP configurations differ only in how they name
the binary — Agent Plugins resolves `./bin/apiary-mcp` against the plugin
root, Claude Code expands `${CLAUDE_PLUGIN_ROOT}/bin/apiary-mcp` — and both
reach the same file.

**The package carries its own binary.** It does not depend on the client
having `apiary-mcp` on its `PATH`, which is what makes it work in a host that
launches a subprocess with a minimal environment.

## Build

From the Apiary repository root:

    make plugin-package

That builds the connector into `bin/` with the same stamping contract as
every other binary in the repository, and then checks the package against both
specifications. The binary is a build product and is not committed.

To check a package that is already built:

    sh scripts/check-plugin-package.sh

## Configure the client

The connector reads its settings from the environment, and falls back to a
settings file for anything the environment leaves unset.

| Variable | Purpose |
| --- | --- |
| `APIARY_MANAGER_ADDR` | ManagerService `host:port` |
| `APIARY_API_KEY` | Dedicated Viewer API key; never put it in this plugin package |
| `APIARY_MANAGER_CA` | Optional PEM CA bundle when the system trust store is insufficient |
| `APIARY_MANAGER_SERVER_NAME` | Optional TLS DNS name when it differs from the address host |
| `APIARY_MCP_ENV_FILE` | Path of the settings file, if not the default |

The settings file is `$XDG_CONFIG_HOME/apiary/mcp.env`, or
`~/.config/apiary/mcp.env` when `XDG_CONFIG_HOME` is unset. It holds one
`KEY=value` per line, and it must be readable only by its owner — the
connector refuses a file that group or others can read, because it may hold
the API key. A missing file is not an error.

The environment wins over the file, and an unexpanded `${...}` placeholder
counts as unset rather than as a value. That last rule matters: a host that
passes a placeholder through verbatim would otherwise make the connector dial
a host named `${APIARY_MANAGER_ADDR}`.

A dedicated API key with the **Viewer** role is required. The connector sends
the key on every call and managerd decides what it may read, so a key with a
higher role would work but is not the intended configuration.

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

In HTTP mode the settings file is not consulted. A connector running as a
daemon takes its configuration from the environment alone, which is the
contract the rc script on a Comb implements.

## Tests

    go test -race ./cmd/apiary-mcp/
    sh scripts/check-plugin-package.sh
    sh scripts/check-mcp-interop.sh

The first runs the real MCP server against a fake ManagerService client. The
second checks the package against both plugin specifications. The third drives
the connector with orcli's real MCP client over both transports.

## Disable

Remove the plugin from the client and revoke its dedicated Viewer API key in
Apiary. The connector does not keep credentials on disk.
