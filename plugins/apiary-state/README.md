# Apiary state plugin

A Claude Code plugin that lets Claude read an Apiary cluster's VM and jail
inventory and cluster health. It is read-only: it runs the `apiary-mcp`
connector (see `plugins/apiary-mcp-connector`), which calls five read RPCs on
ManagerService and nothing else.

This is the Claude Code package. `plugins/apiary-mcp-connector` is the same
connector packaged for a generic MCP host, and the two are alternatives -
install one or the other. Both register an MCP server named `apiary`, so
installing both leaves a host with two servers of the same name. See
ADR-0183 for why they are kept separate.

## Setup

1. Build the connector from the repo root and put it on `PATH`:

       go build -o apiary-mcp ./cmd/apiary-mcp

2. Create a Viewer-role API key on the cluster. The connector sends this key on
   every call, and managerd decides what it may read.

3. Tell the connector where the cluster is. Either set these in the
   environment that launches Claude Code:

   | Variable | Value |
   | --- | --- |
   | `APIARY_MANAGER_ADDR` | ManagerService `host:port`, e.g. `<manager-host>:17700` |
   | `APIARY_API_KEY` | The Viewer key from step 2 |

   ...or put them in a settings file the connector reads:

   | File | Contents |
   | --- | --- |
   | `~/.config/apiary/mcp.env` | `APIARY_MANAGER_ADDR=...` and `APIARY_API_KEY=...`, one per line |

   The file is the more reliable of the two, and it is why the plugin's
   `.mcp.json` carries no `env` block. An `env` block in `.mcp.json` is
   passed to the connector verbatim by some hosts rather than expanded, so
   the connector would receive the literal text `${APIARY_MANAGER_ADDR}` and
   try to dial a host by that name. The connector treats an unexpanded
   `${...}` placeholder as unset and falls back to the file, but the
   cleaner fix is to not declare the block at all.

   The settings file may hold an API key, so it must be readable only by
   its owner. The connector refuses to start if the file is readable by
   group or others.

   The environment wins over the file, so an operator who exports the
   variables still overrides the file.

4. Install this plugin directory in Claude Code.

## Notes

- The connector requires TLS to ManagerService and verifies the certificate. If
  the certificate is not in the system trust store, set `APIARY_MANAGER_CA` to
  its CA bundle and `APIARY_MANAGER_SERVER_NAME` if the name differs from the
  address host. Both are read by the connector itself, and both may be set in
  the settings file too.
- `APIARY_MCP_ENV_FILE` points the connector at a settings file somewhere
  other than `~/.config/apiary/mcp.env`. It is read by the connector itself.
- The settings file is used for the stdio transport only. A connector running
  as a daemon under HTTP mode takes its configuration from the environment
  alone.
- Disable: remove the plugin and revoke the Viewer key on the cluster.
