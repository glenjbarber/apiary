# ADR-0178: The Apiary state plugin for Claude Code

## Status

**Proposed.** This record decides how the read-only `apiary-mcp` connector
is exposed to Claude Code, and where that exposure sits relative to the
`apiary-mcp-connector` package already in the tree.

Supersedes: None
Superseded by: None
Tracking issue: #26
Amends ADR-0162 (the read-only MCP server and its client integrations)

## Context

ADR-0162 built `cmd/apiary-mcp`: a read-only MCP server exposing exactly
three tools - `list_cells`, `get_cell` and `get_cluster_health` - backed
by five read RPCs on ManagerService (`ListVMs`, `ListJails`, `GetVM`,
`GetJail`, `ClusterHealth`), each of which requires the Viewer role.

`plugins/apiary-mcp-connector/` packages that binary for a generic MCP
host. Its manifest is in the agent-plugins.org shape: a `plugin.json` with
a `$schema` and a `com.openai` extension block, an `mcp.json` alongside
it, and a README covering the build step, the Viewer key and the disable
path. That package says nothing about Claude Code in particular.

Claude Code has its own plugin layout, and the two are not the same
thing:

- The manifest lives at `.claude-plugin/plugin.json`, not at the plugin
  root.
- A Claude Code plugin may carry SKILLS - markdown files with frontmatter
  that describe, to a model, how to use the tools a plugin provides.
- The MCP server definition lives in `.mcp.json` at the plugin root.

Nothing in the tree does the Claude Code half. An operator who wants to
ask their coding agent "what is running on Apiary" has to wire the
connector up by hand, and the agent has no written guidance about what
the tools mean or, more importantly, what they must NOT be used for.

## Decision

### 1. `plugins/apiary-state/` is a Claude Code plugin, and it is separate from `apiary-mcp-connector`

They are different packages for different hosts, not two versions of one
thing:

- `apiary-mcp-connector` is the agent-plugins.org package, for a generic
  MCP host. It carries a `com.openai` extension block and a schema URL.
- `apiary-state` is the Claude Code package. It carries a
  `.claude-plugin/plugin.json` manifest and a `skills/apiary-state/SKILL.md`.

Both configure the same `apiary-mcp` binary. That is deliberate, and it
is not duplication of work: there is ONE connector binary (ADR-0162's
"wrap the binary, do not vendor a second copy" decision), and each
package is a thin description of how one host should launch it.

The two are not meant to be installed together. Both register an MCP
server named `apiary`, so a host that loads both has two servers with one
name. Each README says which one it is, and the Claude Code package
names its relationship to the other explicitly.

### 2. The plugin adds a SKILL, and the skill's value is mostly in what it forbids

The connector's own server instructions (ADR-0166) already tell a model
that the tools are read-only and what a tool error means. The skill adds
the parts a SKILL is for: when to reach for these tools at all, and the
reporting discipline that keeps an answer honest.

The rules it states are the ones that keep a read-only cluster view from
producing a false answer:

- Report what the tools return. Quote an error rather than summarising
  it, and never infer a healthy state from a failure.
- A denial that says the key must have the Viewer role is a
  configuration problem. Say so; do not retry it in a loop.
- There are no write tools, and that is intentional - do not offer or
  attempt a change.
- A node's verdict of `unknown` means the check did not answer. Say it
  is unknown; do not call the node down.

The last two matter most. A read-only view of infrastructure invites the
model to suggest remediation, and the honest report of "the check did not
answer" is very different from "the node is down".

### 3. Credentials come from the environment, never from the plugin

`.mcp.json` passes `APIARY_MANAGER_ADDR` and `APIARY_API_KEY` through
from the launching environment. The API key is a real credential, so it
is never committed, and the README says so. The connector requires TLS
to ManagerService and verifies the certificate, so the trust anchor is
the operator's business too: `APIARY_MANAGER_CA` and
`APIARY_MANAGER_SERVER_NAME` are read by the connector itself, and the
README points at both.

### 4. The build step stays explicit

The plugin does not compile anything at launch. The operator builds
`apiary-mcp` from the repository and puts it on `PATH`, exactly as
`apiary-mcp-connector`'s README already says. A plugin that ran `go run`
on first launch would need a Go toolchain on every machine that
installed it, and would compile the same source twice.

## Consequences

- Claude Code can be pointed at an Apiary cluster without hand-wiring an
  MCP server, and the model using it gets written guidance on what the
  three tools can and cannot tell it.
- The connector remains one binary. There is still exactly one
  implementation of the three tools, and ADR-0166's model-agnostic
  property is unchanged - the skill adds guidance, it does not change
  the tool surface.
- The two packages in `plugins/` must be kept in step about the
  connector's environment variables. When `cmd/apiary-mcp` gains a
  setting, both READMEs have to name it.
- A host that installs both packages gets two MCP servers named `apiary`.
  This is documented in both READMEs rather than prevented by the code,
  because the two packages are for different hosts and a host that
  understands both formats is exactly the case where the operator wants
  to choose one.
