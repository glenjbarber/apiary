---
name: apiary-state
description: Read the current state of an Apiary cluster - VM and jail inventory, individual cells, and cluster health - through the read-only Apiary MCP tools. Use when asked what is running on Apiary, where a VM or jail lives, or whether the cluster is healthy.
---

# Reading Apiary state

Use the `apiary` MCP tools. They are read-only: they can list and inspect, and nothing else.

## Tools

- `list_cells` - every VM and jail, with ID, name, type, node, desired state and phase. Capped at 200; `truncated` says when the list is longer.
- `get_cell` - one VM or jail by ID. Requires `type` ("vm" or "jail").
- `get_cluster_health` - each node's verdict. `unknown` means the check did not answer; it is not a healthy result.

## Rules

- Report what the tools return. If a tool errors, say which tool failed and quote the error. Do not infer a healthy state from a failure.
- A tool error that says the key must have the Viewer role means the configured API key is too narrow, or wrong. Say so; do not retry in a loop.
- Do not offer or attempt any change. There are no write tools here, and that is intentional.
- When a node is `unknown`, say that it is unknown and that the answer is not in hand, rather than calling the node down.
