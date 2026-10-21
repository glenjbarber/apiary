# Apiary state plugin

A Claude Code plugin that lets Claude read an Apiary cluster's VM and jail
inventory and cluster health. It is read-only: it runs the `apiary-mcp`
connector (see `plugins/apiary-mcp-connector`), which calls five read RPCs on
ManagerService and nothing else.

## Setup

1. Build the connector from the repo root and put it on `PATH`:

       go build -o apiary-mcp ./cmd/apiary-mcp

2. Create a Viewer-role API key on the cluster. The connector sends this key on
   every call, and managerd decides what it may read.

3. Set these in the environment that launches Claude Code:

   | Variable | Value |
   | --- | --- |
   | `APIARY_MANAGER_ADDR` | ManagerService `host:port`, e.g. `<manager-host>:17700` |
   | `APIARY_API_KEY` | The Viewer key from step 2 |

4. Install this plugin directory in Claude Code.

## Notes

- The connector requires TLS to ManagerService and verifies the certificate. If
  the certificate is not in the system trust store, set `APIARY_MANAGER_CA` to
  its CA bundle and `APIARY_MANAGER_SERVER_NAME` if the name differs from the
  address host. Both are read by the connector itself.
- `${APIARY_MANAGER_ADDR}` and `${APIARY_API_KEY}` in `.mcp.json` are expanded
  from the launching environment. If your Claude Code version does not expand
  them, put the values in the file instead, and do not commit the key.
- Disable: remove the plugin and revoke the Viewer key on the cluster.
