# ADR-0183: The Apiary plugin package

## Status

**Proposed.** This record decides how the read-only `apiary-mcp` connector
is exposed to AI clients, and where that exposure sits relative to the
`apiary-mcp-connector` package already in the tree.

**Section 1 was superseded on 2026-10-30.** It decided there would be two
packages, one per client family. There is one package, satisfying both the
Agent Plugins 1.0.0 specification and the Claude Code plugin format.

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

### 1. There is ONE plugin package, and it satisfies both specifications

**Superseded 2026-10-30.** This section originally decided that
`plugins/apiary-state/` and `plugins/apiary-mcp-connector/` were two
packages for two hosts. That was wrong, and the reason was not visible
until the package was actually installed somewhere.

The Agent Plugins 1.0.0 specification
(<https://agent-plugins.org/specification>) defines a portable package
format, and its technical steering committee includes Amazon, Cursor,
Microsoft, OpenAI and Vercel. The two formats turned out to agree on
almost everything:

| | Agent Plugins 1.0.0 | Claude Code |
| --- | --- | --- |
| Manifest | `plugin.json` at the root | `.claude-plugin/plugin.json` |
| MCP config | `mcp.json` at the root | `.mcp.json` at the root |
| Skills | `skills/<name>/SKILL.md` | `skills/<name>/SKILL.md` |
| Command | `./bin/x`, resolved against the root | `${CLAUDE_PLUGIN_ROOT}/bin/x` |
| Binaries | the package's own | `bin/` is on the client's `PATH` |

So one directory can carry both manifests, both MCP configurations, one
`skills/` tree and one `bin/`, and it loads in either client. The two
manifests differ because the specifications differ; everything else is
shared. `scripts/check-plugin-package.sh` checks the package against both
specifications, so "it loads in either client" is verified rather than
claimed.

**The package carries its own binary.** A bare `command` name is resolved
by the client's platform search rules, which means it depends on the
client's `PATH` — and a host that launches a subprocess with a minimal
environment has no `PATH` entry for it. A plugin-relative `./bin/x`
does not have that dependency. This is not theoretical: the connector
failed to start under orcli's client, which passes exactly `{"PATH=..."}`,
and the fix was to give the package its own copy of the binary.

`plugins/apiary-state/` is gone. The one package is
`plugins/apiary-mcp-connector/`.

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

### 3. Credentials come from the environment or a settings file, never from the plugin

The API key is a real credential, so it is never committed, and the README
says so. The connector requires TLS to ManagerService and verifies the
certificate, so the trust anchor is the operator's business too:
`APIARY_MANAGER_CA` and `APIARY_MANAGER_SERVER_NAME` are read by the
connector itself, and the README points at both.

The connector reads its settings from the environment, and falls back to a
settings file for any setting the environment leaves unset. The file is
`~/.config/apiary/mcp.env` by default, and `APIARY_MCP_ENV_FILE` overrides
the path. It holds one `KEY=value` per line.

**The file exists because a plugin host cannot reliably pass an
environment.** An `.mcp.json` may declare an `env` block, but a host that
does not expand `${...}` placeholders hands the connector the placeholder
text itself, and an explicit `env` block then also overrides whatever was
inherited, so the operator's own exported variables cannot fix it. The
connector would dial a host literally named `${APIARY_MANAGER_ADDR}` and
report the failure as `Unavailable`, which names neither the real cause nor
the remedy. A file is the one channel that does not depend on the host's
variable handling.

**Neither `.mcp.json` nor `mcp.json` declares an `env` block at all.**
Declaring one invites exactly the failure above, and a block that expands
correctly on one host and not another is worse than one that is absent.

**The settings file is found without `$HOME`.** A client that launches a
subprocess with a minimal environment may set neither `$HOME` nor
`PATH`, and `os.UserHomeDir` reads `$HOME`, so the connector consults the
passwd database when `$HOME` is absent. It also honours
`$XDG_CONFIG_HOME`, which is the mechanism every Linux and BSD client
already understands, so a client can point the connector at its own
configuration directory without knowing anything about Apiary.

The connector treats an unexpanded `${...}` placeholder in the environment
as unset rather than as a value, so a host that passes one through verbatim
falls back to the file instead of dialling a host by that name. This is a
backstop, not the design: the design is that the block is absent.

The environment wins over the file, so an operator who exports the
variables still overrides what the file says. A file that group or others
can read is refused, because it may hold the API key; a missing file is not
an error, so a host that configures everything through the environment
keeps working.

**The file is used for the stdio transport only.** A connector running as a
daemon under HTTP mode takes its configuration from the environment alone.
That is not a distinction without a difference: a daemon on a Comb already
has a secret-delivery contract of its own, and the rc script implements it
by reading `/usr/local/etc/apiary/mcp-secrets` and naming every setting on
the command line. Letting a root-owned daemon also pick up a file in root's
home directory would give that deployment a second, undocumented source of
configuration, and ADR-0162 says the connector's configuration comes from
the environment.

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
- A connector launched as a subprocess can now be configured without any
  environment at all, which removes a class of plugin-host failure. It also
  means an API key can live in a file on disk, so the permission check is
  load-bearing rather than a nicety.
- The package satisfies two specifications at once, so a change to either
  one's requirements can break loading in the other client.
  `scripts/check-plugin-package.sh` is what catches that, and
  `make plugin-package` runs it.
