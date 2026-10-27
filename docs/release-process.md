# Apiary's release process

This document is the procedure for cutting an Apiary release. It covers
the version scheme, the tag naming convention, the branch model, and the
release checklist itself.

It is written for Apiary, a Go project, on FreeBSD hosts. Its shape is
adapted from the FreeBSD release-engineering process - the public
`ReleaseEngineering` and `ReleaseEngineering/tasks` wiki pages and the
published FreeBSD 15.0 release schedule - but almost every concrete
step in FreeBSD's process is about an operating-system release: a
version-bump commit across a dozen source files, an installer image, a
`freebsd-update` build for the security team, a branch cut in Subversion.
None of that applies here, and pretending otherwise would produce a
checklist nobody could run. What is kept is the *shape* of the process -
announce, freeze, branch, test, release - and what is dropped is the
ceremony that belongs to a different kind of artifact. Each step below
that does not apply says so and why.

Every claim in this document about the project's own history, its
binaries, or its build is verifiable from this repository. Where
something could not be verified, this document says so rather than
asserting it.

## The two version schemes, and why both exist

There are two different version schemes in play, and conflating them is
the one mistake this section exists to prevent.

### The initial release: `1.0.0-alpha`

The first public release is `1.0.0-alpha`. It is a **software-release
version** in the ordinary SemVer sense: it says what shape the software
is, and nothing about the host it runs on.

`1.0.0` here means "the first release of this software, whose interface
is not yet promised to be stable." It does **not** mean "compatible with
FreeBSD 1.0.0" - there is no such thing, and the mapping in the next
section would be nonsense if it did. The `-alpha` suffix says the
interface may still change in ways that are not backward compatible.

This scheme is *temporary*. It covers the `1.0.0-alpha` release and any
pre-releases that follow it (`1.0.0-alpha.2`, `1.0.0-beta.1`,
`1.0.0-rc.1`). It exists because the project is young and its interface
is still moving, and pretending otherwise would be the more dangerous
choice.

### The eventual scheme: major version = compatibility line

The owner's release checklist says:

> Keep the distinction clear between the initial public release, Apiary
> 1.0.0-alpha, and the eventual compatibility-based release lines such as
> Apiary 15.

and

> Apiary major versions should reflect the lowest compatible FreeBSD
> major version.

So the *eventual* scheme is that an Apiary major version names a
**compatibility line**, and that line is defined by the **lowest
FreeBSD major version** Apiary supports on it. An Apiary release whose
major version is `N` supports FreeBSD `N` and every later FreeBSD major
version, and does not support any FreeBSD major version below `N`.

This is a different *kind* of number from `1.0.0-alpha`. `1.0.0-alpha`
describes a software release. `15.0.0` would describe a **line of
releases that are mutually compatible with each other and with a stated
range of hosts**. The two are not on the same axis, and the checklist's
"keep the distinction clear" is exactly the instruction not to let the
first one silently become the second.

The transition is deliberately deferred. The checklist says:

> After the codebase stabilizes, establish the compatibility-based
> major-version scheme.

and

> Introduce release and stable branches after the codebase stabilizes,
> following the project's release model.

Both of those land **after** the alpha, not before. `1.0.0-alpha` is
released on the plain `main` branch, with no `release/` or `stable/`
branch in existence yet. The compatibility scheme, when it comes,
starts a *new* line - it does not renumber `1.0.0-alpha`, and it does not
retroactively turn the alpha into "Apiary 1".

### What "stabilized" means, concretely

"After the codebase stabilizes" is a real condition, not a mood. This
document defines it as observable, and a release line is not cut until
every one of these holds:

1. **The external interface is declared stable.** The RPC surface
   (`api/rpc/manager.proto`) and the REST surface (`restshimd`) are
   documented as stable, and a change to either is either additive or
   carries an explicit compatibility note. Today neither is declared
   stable; the alpha says so.

2. **The on-disk and raft state formats are declared stable.** A node
   running a newer Apiary can be joined to a Colony running an older
   one, and a snapshot taken by one is restorable by the other. The
   Colony protocol's staged plan (ADR-0156) is complete through restore
   semantics - stages 1, 2 and 3 are merged at the time of writing
   (ADR-0157, ADR-0182, ADR-0188).

3. **The compatibility floor is decided.** The decision between FreeBSD
   14 and FreeBSD 15 as the floor is *open*, and the checklist says so.
   The floor cannot be decided in the abstract: it is decided by a
   working, tested Apiary on that FreeBSD release. ADR-0181 already
   records that the `hastd`/`hastctl` fixes Apiary needed are upstream
   in FreeBSD 15.1-RELEASE, and that 15.1-RELEASE is the supported
   floor.

4. **The release branch model has run at least once.** A `release/`
   branch and a `stable/` branch exist, a release has been cut from
   `release/`, and a patch has been cut from `stable/`. A model that
   has never been exercised is a plan, not a process.

5. **No open question is load-bearing on the public interface.** An open
   question is fine. An open question whose answer changes a published
   API, a stored format, or a supported host is not, because an alpha
   cannot promise compatibility it might have to retract.

Until all five hold, the project stays on the `1.0.0-alpha` scheme and
`main`-only releases. The alpha is explicitly **before** this point.

## Version-to-compatibility-line mapping

The mapping table below is the reference for which Apiary version
belongs to which compatibility line. The "Status" column says which rows
are in force.

| Apiary version | Module path | Compatibility line | Lowest supported FreeBSD | Status |
| --- | --- | --- | --- | --- |
| `1.0.0-alpha` | `github.com/glenjbarber/apiary` | none - pre-compatibility | not yet fixed | **In force.** The initial public release. |
| `1.0.0-alpha.N` | `github.com/glenjbarber/apiary` | none - pre-compatibility | not yet fixed | Reserved for later alphas of the same pre-release line. |
| `1.0.0-beta.N` | `github.com/glenjbarber/apiary` | none - pre-compatibility | not yet fixed | Reserved. |
| `1.0.0-rc.N` | `github.com/glenjbarber/apiary` | none - pre-compatibility | not yet fixed | Reserved. |
| `1.0.0` | `github.com/glenjbarber/apiary` | 1.x (pre-compatibility) | not yet fixed | Reserved. The last release before the scheme changes. |
| `15.0.0` | `github.com/glenjbarber/apiary/v15` | 15.x | FreeBSD 15 | **Reserved.** Not yet established; the floor decision is open. |
| `16.0.0` | `github.com/glenjbarber/apiary/v16` | 16.x | FreeBSD 16 | Reserved. Only if the floor moves. |

The FreeBSD major version in the "lowest supported" column is the
*floor* for that line, not the only version that works. An Apiary 15
line supports FreeBSD 15 and every later FreeBSD major version. A later
FreeBSD is a superset of guarantees, not a new line.

The `15` row is written as an example of the scheme's shape, and the
`15.0.0` in it is **not** a claim that Apiary 15 exists. It is
illustrative, and it is labelled "Reserved" for exactly that reason.

### Why the module path changes with the major version

The `Module path` column is not decoration. Go requires it.

Go's module system treats a module path's **major version suffix** as
part of the path, and it enforces this. For a module path with no
suffix, Go accepts only `v0` and `v1` major versions. The check is in
`golang.org/x/mod/module`:

```go
// module.Check("github.com/glenjbarber/apiary", "v15.0.0")
// -> invalid version: should be v0 or v1, not v15
```

So a tag of `v15.0.0` on the path `github.com/glenjbarber/apiary` is
**not a valid Go module version** and could never be installed. The
compatibility scheme therefore requires the module path to gain a
`/vN` suffix at the same time the major version changes:

| Module path | Valid major versions |
| --- | --- |
| `github.com/glenjbarber/apiary` | `v0`, `v1` only |
| `github.com/glenjbarber/apiary/v15` | `v15` only |
| `github.com/glenjbarber/apiary/v16` | `v16` only |

This is Go's standard mechanism for a module that has to support two
incompatible major versions at once, and it is the mechanism
`golang.org/x/...` modules and every other major-versioned Go project
use. It is also why the alpha's `1.0.0` and the future `15.0.0` cannot
be on the same module path: Go will not let one path carry both.

**Consequence for the transition.** Cutting the first compatibility
line is a **module-path change**, not just a tag. The `go.mod` module
line changes to `github.com/glenjbarber/apiary/v15`, every internal
import path changes with it, and the import path changes in any code
that consumes Apiary as a library. That is a substantial, breaking
change for importers, and it is the correct behaviour - it is exactly
the signal a major version is supposed to send.

This was verified empirically rather than assumed. With the module path
set to `github.com/glenjbarber/apiary/v15` and a `v15.0.0` tag, the
install succeeds and the binary reports `module: v15.0.0`. See
"Verification" below.

### The alpha is not part of a compatibility line

`1.0.0-alpha` sits in the "none" row. It is not "Apiary 1" in the
compatibility sense, and it makes no FreeBSD compatibility promise
beyond "this is what the code in this tag does on the host you run it
on". An alpha that claims a compatibility line would be claiming
something the compatibility floor has not yet been decided for.

## The tag naming convention

### The rule

Tags are `v<MAJOR>.<MINOR>.<PATCH>` with an optional SemVer
prerelease suffix, and **no** build metadata.

| Release | Tag |
| --- | --- |
| The initial public alpha | `v1.0.0-alpha` |
| A later alpha of the same line | `v1.0.0-alpha.2` |
| A beta | `v1.0.0-beta.1` |
| A release candidate | `v1.0.0-rc.1` |
| The first stable 1.0.0 | `v1.0.0` |
| The first compatibility line (reserved) | `v15.0.0` |

### The decision for `1.0.0-alpha`: `v1.0.0-alpha`, not `v1.0.0-alpha.1` and not `v1.0.0-a1`

The brief offered three candidates. All three are valid Go module
versions and all three install; that is not what decides it. The
decision is:

> **The initial public release is tagged `v1.0.0-alpha`.** Subsequent
> pre-releases of the same line increment the *numeric* suffix
> (`v1.0.0-alpha.2`, `v1.0.0-alpha.3`), and the first alpha carries no
> numeric suffix at all.

**Why `v1.0.0-alpha` and not `v1.0.0-alpha.1`.** A numeric suffix on
the *first* prerelease implies a predecessor that does not exist. It
says "alpha 1", which reads as "the first of a series", and invites the
question "where is alpha 0?" - when the truthful answer is that there
was no alpha 0, because the first public release simply *is* the alpha.
`v1.0.0-alpha` says exactly what is true: the alpha of 1.0.0. This is
also the spelling used by Go's own `go` toolchain releases, which went
`go1.0` → `go1.1beta1` → `go1.1rc1` → `go1.1`, i.e. the first beta
carried the bare `beta1` suffix without a leading dot.

**Why not `v1.0.0-a1`.** `a1` is more compact, and it is a legitimate
SemVer prerelease identifier. It is rejected because it is not
self-describing: `alpha` says what stage the release is at, and
`a1` does not. A tag is read in a terminal, in a `git tag` listing, and
in a URL, often months later and by someone who does not know the
project's conventions. `v1.0.0-alpha.2` is legible on sight;
`v1.0.0-a1` requires knowing the convention. The cost of the extra
characters is zero; the cost of the ambiguity is a support question.

**Why not `v1.0.0-alpha.1` as the first tag.** As above - it numbers
from one a series whose first member is the tag itself. If more alphas
follow, the second is `v1.0.0-alpha.2`, and the numbering is
inconsistent with the first tag's own spelling. Either the first tag
carries `.1` and every alpha is numbered, or the first tag carries no
number and numbering starts at `.2`; the second reads more naturally
and matches how `go1.1beta1` treated its first beta.

### Is `v1.0.0-alpha` a valid Go module version? Yes - verified

This is a real constraint, not a stylistic one, so it was checked
rather than reasoned about. The requirement is that a tag which a user
might write in `go install ...@v1.0.0-alpha` actually resolves.

**The check.** Three independent things were verified with Go 1.27.2:

1. **SemVer validity and Go's own module check.** `v1.0.0-alpha` is a
   valid SemVer version, and `module.Check` accepts it for the path
   `github.com/glenjbarber/apiary`.

2. **It resolves through a real module proxy.** A module zip was
   built for the tag, published to a local file-based GOPROXY in the
   layout the proxy protocol specifies, and then:

   ```
   $ go install github.com/glenjbarber/apiary/cmd/apiaryctl@v1.0.0-alpha
   go: downloading github.com/glenjbarber/apiary v1.0.0-alpha
   ```

   The install succeeded, and the resulting binary's own report
   confirmed the version:

   ```
   $ apiaryctl -version
   apiaryctl build=unknown ... commit=unknown built=unknown go=freebsd/amd64
     program:   apiaryctl
     go:        go1.27.2
     module:    v1.0.0-alpha
   ```

   `module: v1.0.0-alpha` is the line that matters: the binary Go
   built *is* the tagged module version, not a guess.

3. **The competing candidates were tested the same way**, and all
   three install cleanly: `v1.0.0-alpha`, `v1.0.0-alpha.1`, and
   `v1.0.0-a1`. The choice between them is therefore made on
   legibility, as described above, and not by a difference in
   installability.

**What `@latest` does, and why the release notes must not promise it.**
The module proxy's `@latest` resolves to the highest **release**
version, and only falls back to the highest prerelease when no release
version exists at all. Both behaviours were observed:

- With only prereleases present, `@latest` resolves to the highest
  prerelease (`v1.0.0-alpha.1` in a test where `v1.0.0-alpha.1`,
  `v1.0.0-alpha` and `v1.0.0-a1` were all published).
- With a release version also present, `@latest` resolves to the
  release, not the prerelease.

So for the alpha, `go install ...@latest` will resolve to the alpha -
but only until a `v1.0.0` release exists, at which point `@latest` will
point there. The release notes say "install `@v1.0.0-alpha`", not "install
`@latest`", because an explicit version is what survives the arrival of
`1.0.0`.

**A caveat on `go install` and this project specifically.** `go install`
resolves a module version and builds it; it does not produce the
*installed* artifact layout Apiary uses on a Comb. Apiary's binaries are
installed to `/usr/local/libexec/apiary/` by `make install`, with
config files in `/usr/local/etc/apiary/`, rc.d scripts in
`/usr/local/etc/rc.d/`, and PAM policy in `/etc/pam.d/` - see
`docs/bootstrap.md`. `go install` also builds only the package named,
in the caller's `GOBIN`, with no `rc.d` script, no sample config and no
PAM policy, and it stamps no build id (a `go install`ed binary reports
`build=unknown`).

`go install` is therefore a **developer and CI convenience**, not the
supported install path for a Comb. The supported path is `make install`
from a checkout of the tag, described in the checklist below. This
distinction is stated here because the tag format was chosen partly for
`go install` resolvability, and it would be a misreading to conclude
that `go install` is the way to deploy Apiary.

**One more constraint, and it is not about the tag.** `managerd` links
`internal/pam`, which is cgo, and so it needs a PAM development
environment to build. On a FreeBSD host that is present. On a machine
that is not, `go install .../cmd/managerd@v1.0.0-alpha` fails to build
with a build-constraints error. Every other binary in `cmd/` is pure Go
and cross-compiles - verified by building each one for
`GOOS=darwin GOARCH=arm64`, where only `managerd` fails. (The README's
"Building and testing" section used to say `cmd/frontend` was the
cgo-bound one; that was stale from before ADR-0087 moved PAM
authentication into `managerd`. The README has been corrected as part
of this work.)

## The branch model

### `main` - the only branch today

`main` is the trunk. Everything is merged into it through a pull
request; nothing is ever pushed to `main` directly. The alpha is cut
from `main`.

`main` is the trunk *until* the compatibility scheme is adopted. At that
point `main` becomes the development branch for the *next* compatibility
line, and the compatibility lines live on their own branches.

### `release/<line>` - the line being prepared

`release/<line>` is where a compatibility line is stabilised. It is cut
from `main` when the line's first release is prepared, and receives
only fixes intended for that line. It is the branch a release is tagged
from.

The rule for cutting it: `release/<line>` is cut from `main` at the
commit where the line's compatibility floor is decided and its first
release candidate is ready, and it is cut **once per line**, not per
release. A `1.5.0` of the same line is cut from `release/1`, not from a
newly cut `release/1.5`.

### `stable/<line>` - the line already released

`stable/<line>` is cut from the tag of a line's first release, and
receives only fixes that are safe for every already-deployed member of
that line. It exists to carry patch releases (`15.0.1`, `15.0.2`) to
operators who have already installed the line.

The rule for cutting it: `stable/<line>` is cut from the `v<line>.0.0`
tag, once that release is published, and not before.

### The order, and where the alpha sits

The full sequence, in the order it happens:

1. `main` only. `1.0.0-alpha` is cut from `main`. **No `release/` or
   `stable/` branch exists.**
2. The code stabilizes (the five conditions above).
3. `release/15` is cut from `main`.
4. `v15.0.0` is tagged from `release/15` and published. The module
   path gains its `/v15` suffix as part of this step.
5. `stable/15` is cut from the `v15.0.0` tag.
6. Patch releases are cut from `stable/15`. Fixes land on `main` first
   and are backported to `stable/15`.

Step 1 is where the project is now, and step 1 is where the alpha
happens. The `release/` and `stable/` branches do not exist at alpha
time, and creating them early would be a branch model with no
exercises behind it.

### Why the alpha does not get its own branch

It would be possible to cut a `release/1.0.0-alpha` branch for the
alpha. It is deliberately not done: the alpha is a single point on
`main`, and a branch that exists for one tag and is deleted after it
carries the cost of a branch model without any of the benefit. The
`release/` branch earns its keep when there are multiple releases on a
line, and the compatibility lines are where that starts.

## The release checklist

This is the runnable sequence for cutting a release. It is adapted from
the FreeBSD release-engineering task list
(`wiki.freebsd.org/ReleaseEngineering/tasks`) and the published FreeBSD
15.0 release schedule.

### What was adapted, and what was dropped

FreeBSD's process, in outline: announce a schedule; hard-freeze the
head branch; bump `X.Y-STABLE` to `X.(Y+1)-PRERELEASE`; prepare release
notes; post `BETA1` and ask for testing; notify the security team so
`freebsd-update` can be built; cut a release branch at `RC1` and bump
`FreeBSDversion`; iterate `RC2`, `RC3`, ... shaking out installation
bugs; for the final release, bump the version, do a test build, tag,
**check out from the tag**, and do the real final build; update a
dozen version-bearing files across the source tree (`src/Makefile.inc1`,
`src/sys/conf/newvers.sh`, `src/UPDATING`, the `mdoc.local` man-page
macros, the release-notes and errata XML, the web site's release
pages, the BSD family tree); and produce install media.

Mapping that onto Apiary:

| FreeBSD step | Apiary equivalent | Notes |
| --- | --- | --- |
| Announce the schedule to developers | Announce the release date and the freeze to the contributors | A message, not a mechanism. No `freebsd-hackers` analogue needed. |
| Hard freeze the head branch | Freeze `main` - no feature commits, only fixes | Enforced by review, not by a `svnadmin/conf/approvers` file. |
| Bump `X.Y-STABLE` → `X.(Y+1)-PRERELEASE` | Bump the version in one place and tag it | See "Where the version lives" below. One value, not a dozen files. |
| Prepare the release notes | Write the release notes for the tag | `docs/release-notes/` holds them. |
| Post `BETA1`, ask for testing | Publish a prerelease tag, ask for testing | A tag and a GitHub release. |
| Notify the security team for a `freebsd-update` build | **Does not apply.** | `freebsd-update` is a binary delta updater for the OS. Apiary has no equivalent, and needs none: see "Why there is no update mechanism". |
| Cut the release branch at `RC1` | Cut `release/<line>` when the line's first RC is ready | Already described above. |
| Iterate RCs, shaking out installation bugs | Iterate RCs, shaking out install and upgrade bugs | Same intent. The install path here is `make install`, so an RC is tested by installing it. |
| Final: bump version, test build, tag, check out from the tag, real final build | Final: bump version, test build, tag, check out from the tag, real final build | **Identical, and deliberately so.** This is the one FreeBSD step that transfers without adaptation, because it is about git rather than about FreeBSD. |
| Update version-bearing files across the tree | Update the version in the one place it lives | Apiary's version is a single link-time value, not a constant in a dozen source files. |
| Produce install media (ISO) | **Does not apply - explicitly skipped.** | The checklist says to skip ISO images for the initial release, and there is nothing to adapt: Apiary is a set of Go binaries installed onto a FreeBSD host that the operator already has. There is no image to build, and building one would mean shipping a FreeBSD installation, which is FreeBSD's job, not Apiary's. |
| Announce to the world | Publish a GitHub release with the notes and checksums | Same intent. |

The steps that were dropped, and why, in one place:

- **The `freebsd-update` build for the security team.** FreeBSD ships
  signed binary deltas so that an administrator can patch a running
  system without reinstalling. Apiary has no equivalent, and adding one
  is not on the table. An Apiary update is: `make update` (or
  `make install`) on each Comb, which the project already has, and
  ADR-0145/ADR-0146 already define a *controlled* update that steps
  the leader aside and updates one Comb at a time. The operational
  requirement the `freebsd-update` build serves is met by a different
  mechanism. What replaces the *build* is a release artifact with
  checksums - see the checklist.
- **The `svnadmin/conf/approvers` hard freeze.** FreeBSD freezes the
  trunk by adding an approvers file that rejects further commits.
  Apiary is on Git, and the freeze is a review rule on `main`: no
  feature commits land between the freeze and the tag. There is no
  file to write.
- **The version bump across a dozen files.** FreeBSD's version is
  baked into the source in many places, so the release engineer edits
  them all. Apiary's version is a single link-time value, so there is
  one thing to set. This is a large part of why the checklist is
  short.
- **The `BETA`/`RC` numbering, per se.** The *shape* - test releases
  before the final one - transfers. The exact number of alphas,
  betas and RCs does not, and is chosen per release rather than fixed
  by a schedule.
- **The `UPDATING` file, errata files, and the release-notes XML.**
  Apiary's release notes are Markdown in this repository, and its
  "errata" surface is the issue tracker and the `docs/audits/`
  records. No XML pipeline to cut.
- **The BSD family tree and the web-site release pages.** Apiary has
  no such tree, and its GitHub releases page is created by the release
  itself.

### Why there is no update mechanism

The FreeBSD step "notify `security-officer@` for a `freebsd-update`
build" has no Apiary equivalent, and this is worth stating plainly
rather than leaving as a gap in a table.

Apiary's daemons are not a running operating system. A `freebsd-update`
delta exists to patch a *running system* in place, safely, without
reinstalling and without a reboot. Apiary's equivalent is already
designed and built:

- `make update` performs a safe sweep across the Colony's Combs, and
  `make force-restart` is a named, per-Comb action for the daemons that
  genuinely need a restart (ADR-0141).
- A *controlled* Colony update steps the leader aside and updates one
  Comb at a time (ADR-0145), and the update may restart the `managerd`
  that is coordinating it (ADR-0146).
- `versioncheck` compares the running binary against the on-disk
  binary, so "is this Comb actually running the new build?" is a
  question with a tool, not a guess.

So the risk the `freebsd-update` build mitigates - a node running code
that does not match what was shipped - is already addressed by
different, working mechanisms. What the release still owes the operator
is a **trustworthy artifact**: a build whose bytes are checkable. That
is the checksum step below, and it is where the release's integrity
effort goes.

### Where the version lives

The version is a single link-time value, injected into
`internal/buildinfo.Version`. The Makefile computes the identity
flags in `scripts/build-ldflags.sh`; that script is the one place that
decides what gets stamped.

**A gap, stated honestly.** As of the commit this document is written
against, `buildinfo.Version` is declared and read, but **no build in
this repository sets it**. `scripts/build-ldflags.sh` stamps
`BuildID`, `BuildTime` and `GitCommit` and never `Version`, so every
binary built by `make` reports no version at all - and `Uninjected()`
returns false for a build that has an id but no version, which is
correct, but a build with no version line in `-version` output is not
something an operator of a *released* binary should have to accept.

This was fixed as part of this work. The `VERSION` variable now flows
from the Makefile through `scripts/build-ldflags.sh` into
`buildinfo.Version`. The default is the version of the git tag on
`HEAD`, or `devel` for an untagged commit; an explicit `VERSION`
overrides it. The mechanism is the existing one - one script, one set
of `-X` flags - not a parallel path.

It is derived from the tag and not from the clock, for the same reason
the build id is: a value that changed on every build would make two
builds of one commit differ, and would break the reproducibility that
`make check-reproducible` proves.

The rule for the release build is therefore: **the tag is the version,
and the build is stamped with it.** A release build is made with
`VERSION=<the tag's version>` and the resulting binary's `-version`
output names that version, so a deployed binary and a git tag can be
checked against each other with `versioncheck`.

### The checklist

The steps below are in order. Steps marked **(release engineer)** are
manual actions; the rest are commands.

#### 1. Decide and announce the release

**(release engineer)** Decide:

- the version (`v1.0.0-alpha`, `v1.0.0-alpha.2`, `v1.0.0-beta.1`,
  `v1.0.0-rc.1`, `v1.0.0`, or a `v15.x.y` line release);
- whether it is a prerelease or a release;
- the freeze date and the target date.

Announce both to the contributors and publicly. FreeBSD announces a
rough schedule and then a reminder; one announcement with both dates is
enough here.

#### 2. Freeze `main`

From the freeze date, `main` takes **fixes only** - no new features, no
refactors that change behaviour, no dependency bumps that move a
published interface. This is FreeBSD's "code slush" and the start of
its "hard freeze", adapted to review-based enforcement.

FreeBSD's freeze is per-branch and the freeze applies to the head
branch while the release branch is cut later. Apiary's alpha is cut
from `main` directly, so the freeze is on `main` for the whole
pre-release.

#### 3. Prepare the release notes

**(release engineer)** Write the release notes for the version, from the
actual commit history, in `docs/release-notes/`. The notes must not
overclaim: the "Not yet implemented" section of `README.md` is the
source of truth for what is not built, and every known limitation
belongs in the notes.

For the initial release, the notes are
[`docs/release-notes/1.0.0-alpha.md`](release-notes/1.0.0-alpha.md).

#### 4. Cut the prerelease tag and ask for testing

**(release engineer)** Tag the prerelease and publish it.

```sh
git tag -a v1.0.0-alpha -m "Apiary 1.0.0-alpha"
git push origin v1.0.0-alpha
```

Then publish a GitHub release for the tag, with the release notes
attached, and say plainly that it is a prerelease: **the interface is
not yet stable, and a later alpha may break it.**

FreeBSD's equivalent is "post BETA1, ask the community for testing".
The ask is the same: get the tag in front of people who will run it
against real hardware.

#### 5. Verify the tag builds and installs, from a clean checkout

This is the step that catches the mistakes that are invisible in
review. Check out the tag into a *fresh* directory and build from it -
FreeBSD's "checkout from the tag, do the real final build", and the
reason it is done from a clean checkout is that a build from a working
tree can contain files the tag does not.

```sh
git clone --branch v1.0.0-alpha --depth 1 https://github.com/glenjbarber/apiary.git
cd apiary
make build
make check-ldflags
make check-stamped
make check-reproducible
```

Each of those targets already exists and each checks a distinct
property:

- `make check-ldflags` - the stamping script produces a complete,
  non-empty set of identity flags. It exists because BSD make's lack of
  `$(shell)` once silently produced empty stamps on every build, and a
  stamp that silently fails is worse than no stamp.
- `make check-stamped` - every binary answers `-version` and its
  identity is classifiable. A binary that cannot name itself is
  refused.
- `make check-reproducible` - the same commit built twice produces the
  same bytes. This is what makes a checksum in the release notes
  meaningful.

Then run the tests, which mirror CI exactly:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
go build ./...
```

**Note on the FreeBSD-only test packages.** `internal/bhyve`,
`internal/cluster`, `internal/hast`, `internal/jail`, `internal/localraft`
and `internal/zfs` shell out to FreeBSD tools and skip their
integration tests off FreeBSD. On a FreeBSD host they run. A release
must be cut on a FreeBSD host, so that is the supported case; the
release engineer should confirm these six packages pass rather than
assume.

#### 6. Install the tag on a real Comb and exercise it

A release is not released until it has been installed the way an
operator installs it. On a scratch Comb:

```sh
make install
make setup
```

then check the identity of what landed:

```sh
/usr/local/libexec/apiary/raftd -version
versioncheck <host>
```

and exercise the documented path: bootstrap a single-node Colony
(`docs/create-node.md`), add a second Comb
(`docs/add-node-to-colony.md`), and confirm the web UI comes up and the
daemons report the expected build. `docs/HANDBOOK.md` is the operator's
guide and is the right thing to follow for this step, because it is
kept true by tests.

This step is the direct equivalent of FreeBSD's "shake out all
installation related bugs at the RC stage", and it is the step that
cannot be skipped, because Apiary's install path touches the host
(`rc.d` scripts, PAM, ZFS, `pf`) in ways a build does not.

#### 7. Publish the release

**(release engineer)** Publish the GitHub release for the tag, with:

- the release notes;
- the **checksums** of the built binaries - this is where the release
  earns the trust that FreeBSD's `freebsd-update` build and signed
  checksum files earn there. Because `make check-reproducible` proves
  the build is reproducible, the published checksum is a fact about the
  source, not a fact about one machine's build. State the Go version
  and the target platform alongside them;
- the supported FreeBSD floor, and the fact that for the alpha it is
  **not yet fixed** (see "What the alpha is and is not" below);
- the installation instruction, which is `make install` from a checkout
  of the tag - **not** `go install`.

For a release that is not a prerelease, the announcement goes out
publicly and the release is no longer moved. For a prerelease, the
announcement says it is a prerelease.

#### 8. Cut the next branches, in the right order

**(release engineer)** Only for the first release of a compatibility
line, not for every release:

- On the first release of a line, `release/<line>` was already cut
  before the release candidate stage.
- After `v<line>.0.0` is published, cut `stable/<line>` **from the
  tag**:

  ```sh
  git branch stable/15 v15.0.0
  git push origin stable/15
  ```

  Cutting it from the tag rather than from the branch head is the
  point: `stable/15` is the line as it was *shipped*, not as it was
  being worked on.

For the alpha, **none of this happens**. There is no `release/` branch
and no `stable/` branch, because the alpha is on the pre-compatibility
scheme.

#### 9. Announce

**(release engineer)** Announce the release, with the notes and the
checksums, and point at `docs/` for the design reasoning.

### What the alpha does *not* include

Stated plainly, because a release checklist that lists only what a
release *does* leaves the reader to assume the omissions are
oversights:

- **No ISO or installation image.** The checklist skips ISO images for
  the initial release, and there is nothing to adapt - Apiary
  installs onto a FreeBSD host the operator already has.
- **No binary delta or `freebsd-update`-style updater.** Covered above.
- **No `release/` or `stable/` branch.** Covered above.
- **No compatibility promise.** The FreeBSD floor is undecided.
- **No `stable/` patch line.** The alpha is a point, not a line.
- **No signed artifacts.** A checksum published beside the artifact is
  what this release offers. Code signing is not in place.

## What the alpha is, and what it is not

An alpha's release notes must not overclaim, and the project's own
`README.md` is the source of truth for what is not built. This section
is the honest summary; the release notes repeat it in the terms a
reader of the notes needs.

### What `1.0.0-alpha` is

- A working, end-to-end Apiary: `raftd` (consensus), `managerd`
  (management and reconciliation), `frontend` (web UI), `restshimd`
  (REST translation), `apiaryctl` (operator CLI), `apiaryinstall`
  (host preflight), `versioncheck` (build-identity check), and
  `apiary-mcp` (a read-only MCP server).
- Real bhyve VM and jail lifecycle management, real ZFS storage, real
  HAST disk replication, real VLAN/DHCP/`pf` network management, a
  real noVNC console, a real web UI.
- A real multi-node Colony: join, membership, leader election, and
  cross-node forwarding for reads, writes, consoles and serial logs.
- A documented design: 185 ADRs in `docs/adr/`, an operator handbook
  in `docs/HANDBOOK.md`, and bootstrap/join runbooks in `docs/`.

### What `1.0.0-alpha` is not

- **Not a stable interface.** The RPC and REST surfaces may change in
  the next alpha. That is what "alpha" means here, and the next alpha
  is allowed to break it.
- **Not a compatibility line.** No FreeBSD floor is promised. The
  decision between FreeBSD 14 and 15 is open.
- **Not high availability.** HAST replication is *data redundancy*;
  automatic failover of a replicated VM is not implemented. This is
  disclosed in the README's "Not yet implemented" section and is
  repeated in the release notes.
- **Not a scheduler.** Nothing decides which Comb a VM should run on
  beyond what a caller sets directly. `MigrateVM`/`MigrateJail` are
  manual, explicit operator actions.
- **Not a hypervisor-portability tool.** Importing VMs from another
  hypervisor has no disk-format conversion, and Apiary is UEFI-only.
  Linux containers have no path at all.
- **Not a supported jail console.** The `jexec`-based interactive jail
  console was built, shipped, and then removed at the user's request;
  it is described in ADR-0068's "Removed" section, and direct host
  access remains the only way to get a shell inside a jail.
- **Not without known gaps.** The README's "Not yet implemented"
  section is the authoritative list, and it is long: no automatic
  failover, no scheduler, no VNC credential encryption on the console
  connection itself, no cleanup of a replica's dataset after a
  record is fully purged, and more. The release notes link to it
  rather than restating it and letting it drift.

### The honest framing

An alpha is a **working system with an unstable interface**, not a
finished product and not a toy. The release notes should say what
works, say what does not, and link to the documents that say so in
detail. They should not describe a roadmap as though it were a
shipped feature, and they should not claim a compatibility guarantee
that has not been decided.

## Verification

The claims in this document that are checkable, and how they were
checked. All of it was run with Go 1.27.2 on FreeBSD/amd64.

| Claim | How it was checked | Result |
| --- | --- | --- |
| `v1.0.0-alpha` is a valid Go module version | `module.Check` / `semver.IsValid` from `golang.org/x/mod` | Valid; `module.Check` returns no error |
| `v1.0.0-alpha` actually installs | A module zip for the tag was published to a file-based GOPROXY, then `go install github.com/glenjbarber/apiary/cmd/apiaryctl@v1.0.0-alpha` | Install succeeded; the binary reports `module: v1.0.0-alpha` |
| `v1.0.0-alpha.1` and `v1.0.0-a1` also install | The same procedure | Both install successfully |
| `@latest` picks the highest prerelease when no release exists | `go list -m ...@latest` against a proxy holding only prereleases | Resolves to the highest prerelease |
| `@latest` prefers a release over a prerelease | The same, with a release version also published | Resolves to the release |
| `v15.0.0` is NOT valid for the unsuffixed module path | `module.Check("github.com/glenjbarber/apiary", "v15.0.0")` | Error: `should be v0 or v1, not v15` |
| `v15.0.0` IS valid for the `/v15` module path | `module.Check` with the suffixed path | Valid |
| The `/v15` path and `v15.0.0` tag really install | A module zip built with the suffixed `go.mod`, published and installed | Install succeeded; the binary reports `module: v15.0.0` |
| The build stamps a version | `make build VERSION=1.0.0-alpha` then `<binary> -version` | The binary reported `build=608e03f5182a version=1.0.0-alpha commit=608e03f5182a built=2026-10-27T15:15:58-04:00 go=freebsd/amd64` |
| An untagged build still says something true | `make build` then `<binary> -version` | The binary reported `version=devel` |
| The stamped version does not break reproducibility | `make check-reproducible` | The two builds of one commit were byte-identical, with the version stamp included |
| The stamping contract is enforced by a test | `go test ./internal/buildinfo/` | Passes; it exercises `scripts/build-ldflags.sh` as a subprocess, not a reimplementation of it |
| The FreeBSD baseline is as described | `wiki.freebsd.org/ReleaseEngineering`, `wiki.freebsd.org/ReleaseEngineering/tasks`, and the published FreeBSD 15.0 release schedule | Fetched and read; the schedule's stages (slush, `stable/15`, ALPHA1-5, `releng/15.0`, BETA1-5, RC1-4, RELEASE) are the baseline the adaptation above follows |

### What could not be verified

- **The FreeBSD 14 vs 15 floor.** The decision is open, by the
  checklist's own statement. This document does not make it.
- **The `release/` and `stable/` branch model in practice.** It is
  specified here but has never been exercised - the alpha is the first
  release, and it is on `main` alone. The first compatibility line
  will be the first real test of it.
- **The `v15.x.y` version numbers themselves.** They are illustrative
  of the scheme's shape. The actual first compatibility line's number
  depends on the floor decision, and the module path would need to
  match it.
- **Whether `go install` remains a supported path at all.** It is
  verified to work for the pure-Go binaries. It is not the deployment
  path and is not proposed to become one.
