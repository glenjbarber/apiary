# Apiary in the FreeBSD Ports Collection

This directory is a **packaging skeleton** for a FreeBSD port
maintainer. It is not a port that has been submitted to, reviewed by,
or accepted into the FreeBSD Ports Collection, and nothing here has
been tested by `poudriere` or `portlint`.

It exists because the project's release checklist calls for
"Distribute Apiary through the FreeBSD Ports Collection", and because
that checklist also carries an open decision:

> The precise Ports naming/package strategy for offering multiple
> versions while allowing only one installed version at a time remains
> to be designed.

That decision is the substantive part of this directory. The rest is
mechanical.

## Layout

    dist/ports/
      README.md            this file
      apiary/              the port itself
        Makefile           the port's Makefile
        pkg-descr          the one-paragraph description
        pkg-plist          the file list the port installs
        distinfo           checksums - PLACEHOLDER, see below
        files/
          README.md.in     the note shown after installation

`dist/ports/` rather than `ports/` because the name says what it is:
a distribution artifact for another project, not a subtree of this
project's own source. Nothing in `dist/` is built by `make build`,
`go build ./...`, or the CI workflow, so it cannot rot into the
build. The port's own `Makefile` is the only file that has to be
correct for anything to happen.

## The multiple-versions strategy

The checklist wants two things that pull against each other:

  * "Support multiple Apiary versions as available choices in the
    Ports tree."
  * "Ensure only one Apiary version can be installed on a given system
    at a time."

These are not in conflict, but only if the *package* names differ
while the *files* do not. FreeBSD's Ports Collection already has a
settled idiom for exactly this shape, and this port follows it rather
than inventing a new one.

### Naming

Each Apiary release line gets its own port directory and its own
package name:

    sysutils/apiary1/       package apiary-1.x
    sysutils/apiary2/       package apiary-2.x

This is the `postgresql14-server` / `postgresql15-server` /
`postgresql16-server` idiom, and it works because the package NAME
carries the version while the port DIRECTORY carries the version too.

The version suffix in the package name is what makes two versions
installable *as choices* but not *side by side*. The package
database knows `apiary-1.2.3` and `apiary-2.0.0` are different
packages, so `pkg install apiary2` can select one and `pkg install
apiary1` the other.

### Only one at a time

Two mechanisms, and both are needed:

  1. **`CONFLICTS_INSTALL`.** Each version's port declares that it
     conflicts with the others at install time:

         CONFLICTS_INSTALL=	apiary1 apiary2

     `pkg` refuses the second install rather than letting two
     versions overwrite each other's files. This is the same mechanism
     `postgresql19-server` uses to keep its own version lines from
     coexisting.

  2. **Identical file paths across versions.** Every version installs
     to the *same* paths: `/usr/local/libexec/apiary/raftd` and
     friends. This is deliberate and it is the reason `CONFLICTS_INSTALL`
     is load-bearing rather than a nicety. If each version installed
     to a version-specific directory, `pkg` would happily install both
     and the operator would end up with two raftd binaries and no way
     to tell which one the rc.d script runs.

     The alternative - version-specific paths like
     `/usr/local/libexec/apiary-1.2/raftd` - was rejected because it
     breaks the rc.d scripts. Those scripts hard-code
     `/usr/local/libexec/apiary/<name>`, because that is the path
     upstream's own Makefile installs them to, and because a Comb's
     binaries are not supposed to move when the operator upgrades.

So: **the package name carries the version, the file paths do not.**
That is what lets the Ports tree offer a choice while the system still
holds exactly one Apiary.

### Upgrading in place

Because the file paths are identical across versions, an upgrade is
`pkg upgrade` (or `pkg install apiary2` followed by `pkg delete
apiary1`, if the operator is moving across a version line).

An upgrade does not restart anything. The project's own `make update`
deliberately restarts only `frontend` and `restshimd`, and refuses to
restart `raftd` or `managerd` unattended, because restarting every
raft voter at once costs the colony its quorum. The same rule applies
here: after a `pkg upgrade`, bring up the new daemons one Comb at a
time.

### The version-to-compatibility-line mapping

This is the part that needs a decision from the maintainer, and this
skeleton proposes one.

Apiary's compatibility line is **the FreeBSD release, not the Apiary
version**. That is the right axis because it is the axis the things
Apiary actually depends on move on:

  * The PAM library. On FreeBSD 15 and earlier, libpam is part of the
    base system. On FreeBSD 16 it is a separate pkgbase package
    (`FreeBSD-pam-dev` provides `/usr/include/security/pam_appl.h`).
    A build against one is not a build against the other.
  * HAST. Apiary's own ADR-0181 records that the two HAST defects
    that used to require a locally patched `hastd` were fixed
    upstream in FreeBSD 15.1-RELEASE. Before that, Apiary needed a
    patched base system, which no port can assume.
  * bhyve, jail, ZFS and pf are all base-system components whose
    interfaces Apiary calls directly, and which change with the
    release.

The proposed mapping:

| Apiary version | FreeBSD floor | Rationale |
| --- | --- | --- |
| 1.x | 15.1-RELEASE | The HAST fixes Apiary depends on are upstream from 15.1 (ADR-0181). |
| 2.x | 16.0-RELEASE | When a 2.x line opens, its floor is the then-current stable, and 2.x is where the pkgbase PAM split is supported. |

The rule behind the table: **a new major version line raises the
FreeBSD floor, and a minor release never lowers it.** A patch release
must build on the same floor as the minor release it belongs to,
because a patch release that silently raised the floor would break
the `pkg upgrade` path for everyone already on it.

This mapping is a proposal, not a decision. The owner of the release
checklist has to confirm it, and the table above is the thing to
confirm.

### What "one installed at a time" does not cover

`CONFLICTS_INSTALL` governs the *package* database. It does not
govern a Comb's Raft state under `/var/db/apiary`, and it is not
meant to. A colony's persisted state is tied to the version line that
wrote it, and moving between lines is a migration, not an upgrade.
The port should not pretend otherwise: `CONFLICTS_INSTALL` is about
not having two binaries fighting over the same paths, and nothing
more.

## The port's build dependencies, and why they are what they are

The port builds with cgo. That is not a preference:

  * `internal/pam` wraps the host's system libpam through
    `github.com/msteinert/pam/v2`, and `cmd/managerd` imports it. A
    `CGO_ENABLED=0` build compiles, but produces a managerd that
    cannot authenticate anybody - a silent failure, which is worse
    than a build error.

The go framework's `CGO_ENABLED` default is 1, and this port leaves
it there.

`libpam` itself needs no `RUN_DEPENDS`: it is a base-system library on
every FreeBSD release this port targets, and the Ports Collection
treats base libraries as always present. `security/pamtester` and
`security/openssh-portable` are the precedent.

The runtime dependencies that ARE ports packages are `dns/dnsmasq` (the
DHCP server Apiary drives) and `emulators/qemu` (for `qemu-img`,
which the ISO store uses to measure an image). ZFS, bhyve, jail and
HAST are base-system components and carry no dependency line.

## What this port deliberately does not do at install time

The project's `make setup` target does three things a package install
should not do, and this port does none of them. Each is called out
here because a reader comparing this to `make setup` will otherwise
think something was forgotten.

  * **It does not write a PAM policy.** `make setup-pam` writes
    `/etc/pam.d/apiary`, but only if it is absent. That file is system
    configuration outside the package's own prefix: a package that
    wrote it could not undo it on `pkg delete`, and a package that
    clobbered it would destroy a hand-edited policy.

  * **It does not generate a TLS certificate.** `make setup-tls` can
    generate one with openssl, but it is deliberately not part of
    `make setup` any more - `apiaryctl install` issues the certificate
    in Go, on every Comb including a single-node one.

  * **It does not run `sysrc ..._enable=YES`.** `make setup-rcd`
    enables the four services. A package that enabled services as a
    side effect of installation would start a quorum member on a host
    the operator had not finished configuring.

It also does not write the four real configuration files. `apiaryctl
install` owns those, and it preserves fields that are already set
rather than overwriting them; shipping a config with the package would
put a second source of truth next to the one the tool owns.

## Placeholders a maintainer must fill in

  1. **`distinfo`.** The `SHA256`, `SIZE` and `TIMESTAMP` lines are
     placeholders with obviously-fake values, and the file explains
     exactly how to compute the real ones. The `go.mod` distfile
     needs its own checksum line as well, because `GO_MOD_DIST=github`
     makes the framework fetch it separately.

  2. **`DISTVERSION`.** `0.1.0a1` is a placeholder for "whatever the
     first release actually is". It must agree with the tag that
     exists upstream.

  3. **`MAINTAINER`.** `ports@FreeBSD.org` is a placeholder. A
     submitted port needs a real maintainer address.

  4. **`LICENSE`.** The port declares `BSD3CLAUSE` with
     `LICENSE_FILE=${WRKSRC}/LICENSE`. That is what this project's
     own LICENSE file is, but a submitted port has to be checked
     against the ports tree's current licence list.

  5. **`COMMIT` and `WWW`.** The comment and the URL are written from
     the project's own README and are accurate as far as they go, but
     a maintainer should check them against the ports convention
     (a `COMMENT` is one line, in the ports house style).

  6. **`APIARY_BUILD_COMMIT`, `APIARY_BUILD_ID` and
     `APIARY_BUILD_TIME`.** These are placeholders. Check out the
     release tag, run `git rev-parse HEAD` and `git log -1
     --format=%cI`, and write the answers in. The port refuses to
     build while the commit is still the placeholder, precisely so
     that a release cannot ship binaries that cannot identify
     themselves.

  7. **The `files/README.md.in` note.** It is accurate today. It
     names `make setup-pam`, which only works if the operator still
     has a source checkout - a maintainer should probably reword that
     to describe creating the PAM policy by hand instead.

  8. **`CATEGORIES`.** `sysutils` is a guess. `sysutils` is what the
     other Go-based system daemons use, but the category is a
     judgement about where a user would look for this.

## Verifying this skeleton

This skeleton was validated against a real FreeBSD Ports Collection
tree (freebsd-ports at `main`, checked out at `/tmp/portstree`), not
just by reading it. The port was installed into `sysutils/apiary` and
built with the framework's own `stage` target.

What was run, and what it showed:

    bmake -V IGNORE        # empty - the framework found nothing wrong
    bmake -V PKGNAME      # apiary-0.1.0.a1
    bmake -V GO_TARGET    # the seven binary targets
    bmake -V GO_VERSION   # 1.27
    bmake stage           # builds all seven binaries, stages the tree

The staged tree is exactly the intended one:

    usr/local/libexec/apiary/{raftd,managerd,frontend,restshimd,
                               apiaryctl,apiaryinstall,apiary-mcp}
    usr/local/etc/rc.d/apiary_{raftd,managerd,frontend,restshimd,mcp}
    usr/local/etc/apiary/{README.md,frontend.json.sample,
                           managerd.json.sample,raftd.json.sample,
                           restshimd.json.sample}
    usr/local/share/licenses/apiary-0.1.0.a1/

and the staged `managerd` was checked to be a FreeBSD binary linked
against `libpam.so.6`, which is the property that makes the whole
thing worth packaging.

Five bugs in this skeleton were found by running it, and not by reading
it:

  * `GO_MODULE` was unset, which silently stopped the go framework
    from vendoring the dependency tree at all - the build would have
    reached the network at build time and would not have been
    reproducible.
  * `USES=go:1.27+:modules` is rejected outright: the framework
    treats a version specifier and `modules` as mutually exclusive
    arguments and sets `IGNORE`.
  * `GO_TARGET` was written as `pkg:binary/path`; the real idiom is
    `pkg:/absolute/path`, with no separate binary-name component.
  * `PLIST_FILES` and `pkg-plist` were both used, which makes the
    framework merge two sources and install every file twice.
  * The sample files were staged as `README.md.sample` and
    `frontend.json.sample.sample`, because the copy step appended
    `.sample` to a name that already ended in it.

A sixth bug is worth calling out because it is the one a reviewer
would most likely have missed: the build stamp. The distfile is a
GitHub source tarball, which has no `.git` directory in it, so
`git rev-parse` inside the extracted tree cannot work and a
post-extract target that asks for the commit stamps `unknown` into
every binary. That produces exactly the state `internal/buildinfo`'s
own comment calls the worst outcome - the binaries install, and every
daemon reports that it does not know what it is. The port therefore
carries the commit in the Makefile, the way `sysutils/go-ntfy` and
`sysutils/nginx-ui` do, and REFUSES to build while that value is
still the placeholder.

What has NOT been verified: a `poudriere` run, and the two runtime
dependencies (`dns/dnsmasq`, `emulators/qemu`) being resolved. Both
require a populated package repository rather than a source tree, and
the framework's dependency check refuses to run without the
dependencies installed - a stock port with a `RUN_DEPENDS` fails the
same way in the same tree.
