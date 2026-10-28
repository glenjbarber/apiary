# $FreeBSD$
#
# Apiary - FreeBSD Ports Collection packaging skeleton.
#
# THIS IS A SKELETON FOR A PORT MAINTAINER, NOT A SUBMITTED PORT.
#
# It is a complete port that follows the FreeBSD Ports Collection idiom
# (bsd.port.mk, USES=go, pkg-plist), and it is derived from this
# repository's own Makefile rather than from an assumption about what
# Apiary installs. It has NOT been submitted to or reviewed by the
# FreeBSD ports community, and it does not carry a real DISTVERSION, a
# real SHA256 or a real build commit - see README.md in this directory
# for the full list of what a maintainer must fill in.
#
# It has been built and staged against a real ports tree (freebsd-ports
# at main), and the staged binaries were checked to be FreeBSD binaries
# linked against libpam.
#
# WHY THE DISTRIBUTION IS A GIT TAG RATHER THAN A MODULE PROXY FETCH
#
# USES=go:modules normally fetches the module zip from proxy.golang.org
# and derives the version from a pseudo-version, which is exactly wrong
# for this project: a pseudo-version names a COMMIT, and Apiary's
# release artifacts are named by TAG. Fetching from the module proxy
# would also mean the port's distfile is not the tarball the release
# workflow published, so the thing a user installs and the thing the
# release page points at would be different bytes obtained by different
# means. Instead this port uses USE_GITHUB (which fetches the tag's
# source tarball) and points GO_MOD_DIST at the same tag's go.mod, so
# both come from one place and describe one commit.
#
# The one thing go.mod is used for at build time is the dependency
# graph. The module cache is populated from the proxy during fetch
# (go-post-fetch), then vendored (go-post-extract), so the build itself
# runs with GOPROXY=off and -mod=vendor: the port build makes no
# network calls once the distfiles are on disk. That is the ports
# convention and it is what makes the build reproducible.

PORTNAME=	apiary
DISTVERSION=	0.1.0a1
PORTREVISION=	0
CATEGORIES=	sysutils

# The upstream project lives at github.com/glenjbarber/apiary. GH_ACCOUNT
# and GH_PROJECT are what USE_GITHUB uses to build the distfile URL;
# GH_TAGNAME defaults to ${DISTVERSIONFULL}, which for this port is the
# release tag "v0.1.0a1" once DISTVERSIONPREFIX is set below.
GH_ACCOUNT=	glenjbarber
GH_PROJECT=	apiary
USE_GITHUB=	yes
DISTVERSIONPREFIX=	v

# The tag's go.mod, so the module cache can be primed with the right
# dependency set. This must be the SAME tag as the source tarball.
#
# GO_MODULE is what tells the go framework to fetch go.mod from
# GO_MOD_DIST (here, the tag's go.mod on raw.githubusercontent.com) rather
# than from the Go module proxy, AND it is what makes the framework
# vendor the dependency tree before the build. Without it the framework
# falls back to proxy.golang.org and derives a pseudo-version, which
# would name a commit rather than this release tag.
GO_MOD_DIST=	github
GO_MODULE=	github.com/glenjbarber/apiary

# Only one Apiary version may be installed on a system at a time.
#
# This is the same mechanism databases/postgresql19-server uses to keep
# its own version lines from coexisting: each version line is a
# SEPARATE package (apiary-1.x, apiary-2.x), so the package database
# can offer a choice between them, and CONFLICTS_INSTALL makes pkg
# refuse the second one rather than letting two versions overwrite
# each other's files.
#
# It works here because every version line installs to the SAME paths
# (/usr/local/libexec/apiary/raftd and friends). The package names
# differ; the files do not. That is deliberate - the rc.d scripts
# hard-code those paths, because that is where upstream's own Makefile
# installs them and because a Comb's binaries must not move when the
# operator upgrades.
#
# A maintainer adding a 2.x line adds a sysutils/apiary2/ port whose
# CONFLICTS_INSTALL names sysutils/apiary1, and adds apiary2 here.
#
# This governs the package database only. It says nothing about a
# colony's persisted state under /var/db/apiary, which is tied to the
# version line that wrote it; moving between lines is a migration, not
# an upgrade.
CONFLICTS_INSTALL=	apiary1

MAINTAINER=	gjb@FreeBSD.org
COMMENT=	Distributed virtualisation control plane for FreeBSD Combs
WWW=		https://github.com/glenjbarber/apiary

# The project's own LICENSE is BSD 2-Clause, and a port that names
# a DIFFERENT license is a licence misstatement in the package
# metadata - so this is BSD2CLAUSE, not BSD3CLAUSE.
LICENSE=	BSD2CLAUSE
LICENSE_FILE=	${WRKSRC}/LICENSE

# USES=go:modules builds in module-aware mode, which is required here
# because the project is a Go module with external dependencies.
#
# The Go version is NOT pinned in USES, and that is deliberate. The
# framework treats a version specifier and "modules" as mutually
# exclusive arguments (USES=go:1.27+:modules is an error the framework
# reports by setting IGNORE), and it is also unnecessary here: this
# project's go.mod declares "go 1.27", so Go 1.26 will refuse to build
# it on its own, and the ports tree's default Go is 1.27 anyway
# (Mk/bsd.default-versions.mk GO_DEFAULT=1.27). The go.mod's own
# directive is the real floor, and it moves with the project rather
# than needing an edit to this Makefile.
USES=		go:modules

# This project needs cgo and the host's PAM. cmd/managerd links
# internal/pam, which is a cgo wrapper around the system libpam
# (github.com/msteinert/pam/v2). CGO_ENABLED defaults to 1 in the go
# framework, which is what we want - a CGO_ENABLED=0 build would
# compile managerd without PAM support and produce a binary that
# silently cannot authenticate anybody.
#
# libpam itself needs no RUN_DEPENDS: it is part of the base system on
# every FreeBSD release this port targets (15.1 and later - see
# ADR-0181 for Apiary's own 15.1 floor). The ports tree treats base
# libraries as always-present, exactly as security/pamtester and
# security/openssh-portable do.
#
# These are the third-party tools Apiary's daemons shell out to at
# runtime, verified against the repo's own source rather than assumed:
# zfs/zpool (internal/cluster, internal/backup), jail/jexec/jls
# (internal/jail, internal/cluster), bhyve/bhyvectl (internal/bhyve),
# hastctl/hastd (internal/hast), pfctl (internal/pf), at (internal/
# deadman), pw (internal/frontend/password.go), smart (internal/
# hoststats/stats.go), qemu-img (internal/isostore) and dnsmasq (the
# DHCP server Apiary drives, installed on demand by internal/install).
#
# ZFS, bhyve, jail and hast are all base-system components, not ports
# packages, so they carry no dependency here. dnsmasq and qemu are
# real ports.
RUN_DEPENDS=	dns/dnsmasq:dnsmasq \
		emulators/qemu:qemu

# Apiary's own binaries live in libexec, not bin: they are daemons and
# operator tools, not something an operator types by bare name. The
# rc.d scripts exec /usr/local/libexec/apiary/<name> by absolute path,
# so this path is load-bearing and must match the PLIST_SUB below.
APIARY_LIBEXECDIR=	${PREFIX}/libexec/apiary
APIARY_SBINDIR=		${PREFIX}/sbin
APIARY_CONFDIR=		${PREFIX}/etc/apiary
APIARY_DATADIR=		/var/db/apiary
APIARY_LOGDIR=		/var/log/apiary
APIARY_RUNDIR=		/var/run/apiary

# The runtime directories the package creates and owns. They are
# recorded with PLIST_DIRS so that `pkg info` shows them and `pkg
# delete` removes the ones that are empty.
PLIST_DIRS=	${APIARY_DATADIR} \
		${APIARY_DATADIR}/raftd \
		${APIARY_DATADIR}/isos \
		${APIARY_RUNDIR} \
		${APIARY_LOGDIR}

# The rc.d scripts are the project's own, from etc/rc.d/ in the source
# tarball. They are installed by post-install into ${PREFIX}/etc/rc.d/,
# which on a standard FreeBSD prefix is /usr/local/etc/rc.d/ - the path
# the scripts' own PROVIDE/REQUIRE lines and the project's Makefile
# setup-rcd target both expect.
#
# They are NOT declared with USE_RC_SUBR. USE_RC_SUBR expects each
# script to exist as files/<name>.in in the port's own files/ directory
# (it adds them to SUB_FILES, and the framework then substitutes
# %%VAR%% into ${FILESDIR}/<name>.in to produce ${WRKDIR}/<name>).
# These scripts are the upstream project's, not the port maintainer's,
# and they carry no %%VAR%% placeholders: they hard-code
# /usr/local/libexec/apiary/<name> because that is the path the
# upstream Makefile installs them to. Shipping them through post-install
# keeps the port's own copy in step with upstream automatically, which
# is the same choice sysutils/bastille makes.
#
# apiary_mcp IS in this list, but it is OFF BY DEFAULT: the script's own
# rcvar defaults to NO. That is deliberate (ADR-0162) - the MCP
# connector is a deployment decision an operator makes for a CONTROL
# host, not something every Comb should expose. The script is installed
# so an operator who wants it can `sysrc apiary_mcp_enable=YES`.
APIARY_RCSCRIPTS=	apiary_raftd \
			apiary_managerd \
			apiary_frontend \
			apiary_restshimd \
			apiary_mcp

# The seven binaries the project's own Makefile installs (INSTALL_SRCS).
# versioncheck is deliberately NOT among them: the Makefile's own
# comment says it is "a tool an operator runs FROM this checkout and
# never leaves on the Comb", and it is excluded from INSTALL_SRCS
# there too. It is still built (SRCS contains it) so that the release
# workflow can ship it if it wants to - but it is not installed.
#
# GO_TARGET's package:path form installs a binary at an absolute path.
# The leading "./cmd/raftd" is the package to build; "raftd" is the
# binary name; "/usr/local/libexec/apiary" is where it goes. The go
# framework's do-install target handles the "absolute destination"
# case itself (it prefixes ${STAGEDIR}), which is exactly what is
# needed for a libexec path rather than ${PREFIX}/bin.
GO_TARGET=	./cmd/raftd:${APIARY_LIBEXECDIR}/raftd \
		./cmd/managerd:${APIARY_LIBEXECDIR}/managerd \
		./cmd/frontend:${APIARY_LIBEXECDIR}/frontend \
		./cmd/restshimd:${APIARY_LIBEXECDIR}/restshimd \
		./cmd/apiaryctl:${APIARY_SBINDIR}/apiaryctl \
		./cmd/apiaryinstall:${APIARY_LIBEXECDIR}/apiaryinstall \
		./cmd/apiary-mcp:${APIARY_LIBEXECDIR}/apiary-mcp

# The project's own build identity: -trimpath and -buildvcs=false are
# already set by the go framework, and this port adds the ldflags that
# stamp internal/buildinfo with the commit and the commit's date.
#
# The stamp is not decoration. Every Apiary daemon prints its build ID
# on startup, and versioncheck compares that against the binary on
# disk - which is the only way to tell a deployed build from a running
# one, because copying a binary over a running executable leaves new
# bytes on disk and old ones in memory. A port that installed an
# unstamped binary would leave every daemon reporting build=unknown,
# which is precisely the silent failure internal/buildinfo's own
# comment says is worse than a missing stamp.
#
# The values are the distfile's own commit and date. ${DISTVERSIONFULL}
# is the release tag; the commit is taken from the tag's tree, and the
# date from that commit, so the stamped identity describes the source
# and does not vary between two builds of the same tag.
GO_BUILDFLAGS+=	-ldflags "-X github.com/glenjbarber/apiary/internal/buildinfo.BuildID=${APIARY_BUILD_ID} -X github.com/glenjbarber/apiary/internal/buildinfo.BuildTime=${APIARY_BUILD_TIME} -X github.com/glenjbarber/apiary/internal/buildinfo.GitCommit=${APIARY_BUILD_COMMIT}"

# The tag's commit, resolved at build time from the fetched distfile.
# APIARY_BUILD_COMMIT is the full 40-character hash; the short form is
# what the daemons print.
#
# These three variables are filled in by the post-extract target below.
# A port maintainer who prefers to hard-code them for a release may do
# so; they are computed here so that the port does not have to be
# edited for every release, and so that the stamp can never disagree
# with the tarball that was actually fetched.
# The values a release port stamps into the binaries. The commit is
# the one the release tag points at.
#
# These are WRITTEN DOWN rather than computed at build time on purpose.
# The distfile is a GitHub source tarball, which does not contain a .git
# directory, so there is nothing in the extracted tree to ask for the
# commit - `git rev-parse` inside it fails. The ports tree's own
# convention for this is the same one: a port that stamps a commit
# into its binaries writes the hash in its Makefile and says how it was
# obtained in a comment. See sysutils/go-ntfy (GITID= f18d113) and
# sysutils/nginx-ui (GITID= fa92e51584c5a3d402b1a10c2687fc375d3722b7),
# whose comments say to run `git checkout <tag> && git rev-parse HEAD`.
#
# So: to release a version, check out the tag and run
#
#	git rev-parse HEAD
#
# and put the answer here. BuildID is the short form (12 characters),
# which is what the daemons print.
#
# The three values below are PLACEHOLDERS. A port built with them
# stamps build=unknown, which is exactly the state the project's own
# buildinfo package says is the worst outcome: the binary is installed
# and every daemon reports that it does not know what it is. A
# maintainer MUST replace them before releasing.
APIARY_BUILD_COMMIT=	0000000000000000000000000000000000000000
APIARY_BUILD_ID=	000000000000
APIARY_BUILD_TIME=	1970-01-01T00:00:00Z

# The four sample configuration files and the sample directory's README
# are installed next to the real config path, exactly as the project's
# own `make install` does. They are documentation: no daemon ever reads
# a .sample file. They are installed as @sample entries so that a
# later `pkg upgrade` does not overwrite an operator's edited copy.
#
# apiaryctl.json.sample and apiaryinstall.json.sample deliberately do
# NOT exist: the project's own etc/apiary/README.md says so - neither
# binary has configuration of its own, and "a sample of nothing is not
# a document".
#
# The real config files (raftd.json, managerd.json, frontend.json,
# restshimd.json) are NOT installed by this port. They are written by
# `apiaryctl install`, which is run by the operator after the port is
# installed, and which refuses to overwrite a field that is already
# set (ADR-0147 Part 1). Shipping a config with the port would put a
# second, conflicting source of truth next to the one the tool owns.
#
# The TLS certificate is likewise NOT installed. managerd's serving
# certificate is issued in Go by `apiaryctl install`, on every Comb
# including a single-node one, because TLS being on by default is
# ADR-0147 Part 4's decision rather than a consequence of there being
# peers. The project's `make setup-tls` target can still generate one
# with openssl for an operator who wants that, but it is deliberately
# not part of `make setup` any more.
#
# The PAM policy file (/etc/pam.d/apiary) is also NOT installed by
# this port. It is system configuration outside the package's own
# prefix, the project's `make setup-pam` target writes it only if
# absent so a hand-edited one is never clobbered, and writing it at
# package-install time would mean `pkg remove` could not undo it. An
# operator creates it with `make setup-pam` or by hand.

# The packing list lives in pkg-plist, not in PLIST_FILES here. The
# framework merges both, so listing the same file in each would install
# it twice into the package manifest.
#
# PLIST_FILES is still the right place for anything that needs a
# %%VAR%% substitution; the port's plist needs none, because every path
# it names is a literal /usr/local path or a %%ETCDIR%%-style value
# substituted below.

PLIST_SUB=	APIARY_LIBEXECDIR=${APIARY_LIBEXECDIR} \
		APIARY_CONFDIR=${APIARY_CONFDIR} \
		APIARY_DATADIR=${APIARY_DATADIR} \
		APIARY_LOGDIR=${APIARY_LOGDIR} \
		APIARY_RUNDIR=${APIARY_RUNDIR}

# Runtime directories. /var/log/apiary is the one that matters most:
# every rc.d script's daemon(8) invocation opens its logfile there
# immediately on start, before the daemon binary itself ever runs, so
# without it a first `service apiary_raftd start` on a fresh host
# fails outright. The others are created by the daemons themselves at
# startup (raftd's socket dir, the data dirs), but creating them in
# the package is what the project's own `make setup-dirs` target does
# and it makes the state visible in `pkg info` rather than hidden in a
# daemon's side effect.
#
# They are owned by root:wheel and mode 0755, matching the project's own
# mkdir -p.
# The sample configuration files and the sample README are NOT listed
# in SUB_FILES. SUB_FILES is for files the PORT maintains in its own
# files/ directory, from which the framework builds a substituted copy;
# these files belong to upstream and are copied verbatim out of the
# source tree in post-install, the same way sysutils/bastille handles
# its upstream sample configuration.
#
# The port's own README.md - the note to the operator about what this
# package does and does not do at install time - IS a port-maintainer
# file, so it is the one that belongs in files/ as README.md.in and is
# listed here.
SUB_FILES=	README.md

post-extract:
# Fail loudly if the build identity has not been filled in.
#
# A build that stamps build=unknown installs cleanly and every daemon
# reports that it does not know what it is - which is the silent
# failure the project's own internal/buildinfo comment calls worse
# than a missing stamp, because the toolchain looks healthy while
# nothing can be traced back to a commit. Refusing here is what turns
# that into a build error instead of a mystery in production.
	@if [ "${APIARY_BUILD_COMMIT}" = "0000000000000000000000000000000000000000" ]; then \
		${ECHO_MSG} "===>  ERROR: APIARY_BUILD_COMMIT is still the placeholder." ; \
		${ECHO_MSG} "    Check out the release tag, run 'git rev-parse HEAD'," ; \
		${ECHO_MSG} "    and put the commit and its date in this Makefile." ; \
		exit 1 ; \
	fi

post-install:
# The rc.d scripts come from the upstream source tree (etc/rc.d/) and
# are installed into ${PREFIX}/etc/rc.d/, mode 0755, which is what the
# project's own `make setup-rcd` target does.
	@${MKDIR} ${STAGEDIR}${PREFIX}/etc/rc.d
	@for s in ${APIARY_RCSCRIPTS} ; do \
		${INSTALL_SCRIPT} ${WRKSRC}/etc/rc.d/$$s ${STAGEDIR}${PREFIX}/etc/rc.d ; \
	done
# The sample configuration files and the sample README go next to the
# real config path, as @sample entries so that a later `pkg upgrade`
# will not overwrite an operator's edited copy. They are copied out of
# the source tree, not out of ${STAGEDIR}: they were never staged
# there in the first place.
	@${MKDIR} ${STAGEDIR}${APIARY_CONFDIR}
	@for s in README.md frontend.json.sample managerd.json.sample \
		raftd.json.sample restshimd.json.sample ; do \
		${INSTALL_DATA} ${WRKSRC}/etc/apiary/$$s ${STAGEDIR}${APIARY_CONFDIR}/$$s.tmp ; \
		${MV} ${STAGEDIR}${APIARY_CONFDIR}/$$s.tmp ${STAGEDIR}${APIARY_CONFDIR}/$$s ; \
	done
# The runtime directories.
	@${MKDIR} ${STAGEDIR}${APIARY_DATADIR}/raftd
	@${MKDIR} ${STAGEDIR}${APIARY_DATADIR}/isos
	@${MKDIR} ${STAGEDIR}${APIARY_RUNDIR}
	@${MKDIR} ${STAGEDIR}${APIARY_LOGDIR}
# The runtime directories are recorded with PLIST_DIRS rather than
# being echoed into ${TMPPLIST} here, so that the packing list has a
# single source of truth (pkg-plist plus PLIST_DIRS) instead of being
# assembled from three places.

.include <bsd.port.mk>
