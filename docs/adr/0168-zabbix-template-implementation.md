# ADR-0168: The Zabbix template, as a checked artefact

## Status

**Proposed.** This records the implementation of ADR-0161's read-only
Zabbix integration as a template artefact plus the test that keeps it
honest. It implements ADR-0161; it does not replace it.

Supersedes: None
Superseded by: None
Tracking issue: #24
Implements: ADR-0161 (Zabbix monitoring integration), ADR-0160 (the
monitoring REST surface the template reads)

## Context

ADR-0161 decides that Zabbix pulls from Apiary, that the integration is
read-only, that the credential is a Viewer-scoped API key held in a
Zabbix secret macro, that TLS validation is required, and that the item
keys are stable and named. It is a design record. As of this ADR,
none of it was implemented: there was no template, no script, and no
test.

That gap is smaller than it looks, because the pieces ADR-0161 depends
on already exist. ADR-0160's monitoring API landed as PR #184 and
provides `GET /v1/monitoring/health` and `GET /v1/monitoring/nodes`,
both Viewer-tier and both pull-only. This ADR therefore implements
only the Zabbix-side artefact and the checks around it.

The reason the checks are the substance of this ADR, rather than an
afterthought, is that a Zabbix template fails SILENTLY. A template
with a typo in a macro name does not error; it collects nothing. A
template that reads the wrong JSON path does not error; it collects
the wrong number, or the same number for every host. A template with
TLS verification left at Zabbix's default does not error; it accepts
any certificate. Every one of those failures looks like working
monitoring until someone notices the number is wrong.

## Decision

The integration ships as a Zabbix 7.0 template export at
`etc/zabbix/apiary-zabbix-template.yaml`, read and checked by
`internal/zabbixtemplate`.

**The template polls the Colony once per interval.** One HTTP agent
item, `apiary.cluster.report`, reads `GET /v1/monitoring/health` and is
the master for everything else. The cluster verdict and every per-Comb
item are dependent items on it, so a Colony of fifty Combs costs one
request per interval rather than fifty. The discovery rule is a
dependent rule on the same master, so the Combs are enumerated from
the report the poll already fetched.

**Each per-Comb item selects its own row.** A per-Comb item's
preprocessing path is
`$.nodes[?(@.node_id == "{#NODEID}")].<field>.first()`. This is the
detail the template's correctness rests on, and it is worth stating
why: the master item is the WHOLE report, so a path of `$.health.status`
would resolve against the colony's top-level verdict, and every Comb
would report the colony's verdict. A path of
`$.nodes[*].health.status` would resolve against the array and return
an indefinite list, which Zabbix rejects as a dependent item's value.
Only the filter form selects one row and returns one value.

**The LLD macro is `{#NODEID}`**, matching ADR-0161's item keys
exactly. The macro name is not cosmetic: it is interpolated into the
item key, and a different name would produce keys that do not match
the ones the ADR documents.

**TLS verification is stated explicitly.** Zabbix's own schema default
for `verify_peer` and `verify_host` is OFF, so a template that omits
them disables certificate and hostname validation. The template sets
both, and the test checks both.

**The credential is a `SECRET_TEXT` macro**, `{$APIARY.API_KEY}`, with
no value in the committed file. That is what keeps Zabbix from
printing the key in the frontend, in an exported YAML, or in a log
line. The macro carries no default: a template with a default value is
a template with a committed credential.

**The template is read-only.** Every item is an HTTP agent or a
dependent item; there is no script, no trapper, and no command item, so
nothing in it can change Apiary's state. The test asserts the item
types rather than trusting the file to be well-behaved.

**Missing data stays missing.** Apiary's monitoring report omits a
measurement it did not take rather than rendering it as zero, and the
template preserves that: a JSONPath that matches nothing produces no
value, which Zabbix records as unsupported rather than as 0. The
"worst pool capacity" item takes `max()` across the pools rather than
an average, because an average would hide the pool that is actually
filling.

**An unreachable Comb and an unhealthy Comb are different triggers.**
The health trigger fires when the verdict is something other than
healthy; a separate trigger fires when the observations stop arriving.
Collapsing them means one of the two problems loses its alarm, and the
one that collapses is always the one that pages an operator during
every monitoring outage.

### What the test checks, and why each one exists

`internal/zabbixtemplate` parses the committed template and asserts
properties of it. The checks are deliberately properties rather than
the file's exact shape, so that a harmless reordering does not fail
the build while a real defect does:

  - Every item key ADR-0161 names exists, and every key the template
    has is one the ADR names. Both directions: a missing key is a
    silently unmonitored metric, and an extra key is a contract nobody
    agreed to.
  - The LLD macro is the one the ADR names.
  - The credential macro is `SECRET_TEXT` and carries no value.
  - TLS peer and host verification are enabled on every item that
    reads a URL.
  - Every URL is HTTPS, through the `{$APIARY.URL}` base macro.
  - The template reads ADR-0160's monitoring endpoints.
  - Every item type is read-only.
  - Every URL-bearing item sends the credential in an `Authorization`
    header.
  - Every per-Comb item selects its own row from the report.
  - The unavailable trigger and the unhealthy trigger both exist.
  - Exactly one item performs an HTTP request.

The test reads the template from a path derived from its own source
location rather than a hard-coded one, so it passes on a machine that
did not write it.

## Consequences

- A Zabbix operator can import the template, point `{$APIARY.URL}` at
  a Comb's REST shim, and supply a Viewer API key, and get per-Comb
  health, load, memory, worst-pool capacity and worst-disk-health
  items without writing any Zabbix configuration.
- The template and ADR-0161 cannot drift apart silently: a change to
  one that is not made to the other fails the build.
- Zabbix owns scheduling, retention, history and trigger evaluation.
  Apiary retains no monitoring history, so the polling interval IS the
  history resolution, and the template says so in the macro's
  description.
- Numeric thresholds for load, memory, pool capacity and disk health
  remain the operator's choice. The template ships no threshold that
  would encode a site-specific judgement as an Apiary default.
- The template is a read-only core artefact. Anything that writes -
  remediation, or a Zabbix-driven Apiary mutation - is out of scope
  and would be a separate decision under ADR-0161's product policy.
- The template has not been exercised against a live Zabbix server in
  this repository. It was written against Zabbix 7.0's own export
  schema, taken from the export builder in the Zabbix source tree and
  from shipped 7.0 template exports. An import into a real Zabbix
  server is the next verification step and is not claimed here.

## Alternatives and consequences

- **Push from Apiary to Zabbix.** Rejected by ADR-0161: it would put
  a Zabbix credential in every Apiary installation and tie collection
  to leader changes.
- **One HTTP agent item per Comb.** Rejected here: it makes the poll
  cost grow with the Colony, for no benefit, since one report already
  carries every row.
- **A Zabbix-side script item that renders the report.** Rejected: it
  would move the field mapping out of the template's declarative
  items and into code that Zabbix would have to run, for no gain.
- **Checking the template by hand against the ADR.** Rejected: the
  checks are mechanical, and the failures they catch are silent.
