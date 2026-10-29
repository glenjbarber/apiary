PORTNAME=	apiary
DISTVERSIONPREFIX=	release/
DISTVERSION=	1.0.0.a0
CATEGORIES=	net-mgmt

MAINTAINER=	gjb@FreeBSD.org
COMMENT=	Distributed virtualization control plane for FreeBSD Combs
WWW=		https://apiary.work

LICENSE=	BSD2CLAUSE
LICENSE_FILE=	${WRKSRC}/LICENSE

# git is a BUILD dependency only: the build reads the commit id out of
# the distfile's pax global header with "git get-tar-commit-id", which
# is the exact value the tarball carries rather than a copy of it.
# @lite is the small build-only flavor; the full git is not needed to
# read one header out of a tarball.
BUILD_DEPENDS=	git:devel/git@lite
RUN_DEPENDS=	dnsmasq:dns/dnsmasq
# The bhyve UEFI firmware is deliberately NOT a dependency.
#
# bhyve itself is in the base system, but the .fd file bhyve boots from
# is a port - and the port that carries it is a METAPORT whose real
# payload is sysutils/edk2@bhyve, which is amd64-only. A hard
# RUN_DEPENDS on it would make this package uninstallable on aarch64,
# where bhyve cannot run at all and the firmware is meaningless.
#
# The firmware is also not needed to run Apiary: it is needed only to
# BOOT a VM, and only on a Comb that hosts VMs. A control host, and any
# Comb that has not yet been given a VM to run, has no use for it.
# apiaryctl install reports the firmware as a need when a Comb actually
# wants it, rather than the package insisting on it up front.

USES=		go:modules
USE_GITHUB=	yes
GH_ACCOUNT=	glenjbarber
GH_PROJECT=	apiary

# The rc.d scripts are installed by the ports framework, not by a
# hand-rolled post-install loop. USE_RC_SUBR names the
# files/<name>.in templates in FILESDIR; the framework expands their
# %%PREFIX%% markers, installs them under ${PREFIX}/etc/rc.d, and adds
# them to the packing list itself. That is why no etc/rc.d entry
# appears in pkg-plist: the framework owns those, and a hand-written
# copy would fight it.
#
# It also matters that these are SUB_FILES and not PLIST_FILES: the
# framework only substitutes %%VAR%% in a file it processes itself, so
# a script listed directly in pkg-plist would ship with its %%PREFIX%%
# markers still in it.
USE_RC_SUBR=	apiary_raftd apiary_managerd apiary_frontend apiary_restshimd \
		apiary_mcp

GO_MOD_DIST=	github
GO_MODULE=	github.com/glenjbarber/apiary

# Apiary's own binaries live in libexec, not bin: they are daemons and
# operator tools, not something an operator types by bare name. The
# rc.d scripts exec ${PREFIX}/libexec/apiary/<name> by absolute path,
# so this path is load-bearing and must match the PLIST_SUB below.
_APIARY_LIBEXECDIR=	${PREFIX}/libexec/apiary
_APIARY_SBINDIR=	${PREFIX}/sbin
_APIARY_CONFDIR=	${PREFIX}/etc/apiary
_APIARY_DATADIR=	/var/db/apiary
_APIARY_LOGDIR=		/var/log/apiary
_APIARY_RUNDIR=		/var/run/apiary

GO_TARGET=	./cmd/apiary-mcp:${_APIARY_LIBEXECDIR}/apiary-mcp \
		./cmd/apiaryctl:${_APIARY_SBINDIR}/apiaryctl \
		./cmd/apiaryinstall:${_APIARY_LIBEXECDIR}/apiaryinstall \
		./cmd/frontend:${_APIARY_LIBEXECDIR}/frontend \
		./cmd/managerd:${_APIARY_LIBEXECDIR}/managerd \
		./cmd/raftd:${_APIARY_LIBEXECDIR}/raftd \
		./cmd/restshimd:${_APIARY_LIBEXECDIR}/restshimd

# THE BUILD STAMP
#
# Apiary's binaries carry their own identity: which commit they were
# built from, and when that commit was made. versioncheck compares that
# stamp against the binary on disk to answer "is the running process
# the build I installed?", and every daemon prints its own stamp on
# startup. A package built without the stamp installs, and every daemon
# then reports build=unknown - which looks like a working feature and
# answers nothing.
#
# The stamp is derived from the DISTFILE rather than hard-coded here.
# Hard-coding it works exactly until the next release: the Makefile
# would still name the previous release's commit, the package would
# build, and the binaries would report a commit they were not built
# from - a false claim, in the one field whose entire purpose is to be
# true. Deriving it means the stamp is right by construction for
# whatever tag the distfile came from, and there is nothing to update
# when the next release is cut.
#
# GitHub's source archives carry the commit they were made from in the
# tarball's pax global header, and git(1) reads it back out with
# "git get-tar-commit-id". That is where the value comes from: it is
# read out of the distfile, so the port's claim about the tarball and
# the tarball itself cannot drift.
#
# The values are read in pre-build, not when this Makefile is parsed:
# the distfile does not exist yet at parse time, so a parse-time read
# would produce an empty commit and stamp the package with it.
# pre-build runs after the distfile has been fetched and unpacked,
# which is the first moment the tarball is readable.
#
# The three variables below are RECURSIVE (=, not :=) and they read the
# file pre-build wrote. That is what makes them work: make expands a
# recursive variable at the moment it is USED, and the use is inside
# the build recipe, which runs after pre-build. A := assignment would
# have been expanded at parse time, before the file existed, and would
# have produced an empty commit.
#
# The date is read from the tarball's member mtimes, which GitHub sets
# to the commit's own timestamp - so this is the commit's date, not the
# moment the tarball was built. It is rendered in UTC; the project's own
# release workflow renders the same instant in the committer's local
# timezone, so the two stamps differ in their spelling and not in the
# instant they name.
#
# "tar -tvf" would be simpler for the date, but it prints the time to
# the MINUTE and in a local timezone, and buildinfo wants seconds and
# RFC3339 - so stat(1) reads the extracted tree's mtime as an epoch and
# date(1) formats it.
#
# A stamp that names no commit is worse than no stamp, because it looks
# like a stamp. The build refuses to run rather than produce one.
pre-build:
	@commit=$$(gzip -dc ${DISTFILES} 2>/dev/null | git get-tar-commit-id) ; \
	if [ -z "$$commit" ] ; then \
		${ECHO_MSG} "===>  ERROR: the distfile carries no commit id." ; \
		${ECHO_MSG} "    The build stamp is derived from the tarball's pax" ; \
		${ECHO_MSG} "    global header, and this one has none." ; \
		exit 1 ; \
	fi ; \
	time=$$(stat -f '%m' ${WRKSRC} 2>/dev/null | \
		${XARGS} -I{} date -u -r {} +%Y-%m-%dT%H:%M:%SZ) ; \
	if [ -z "$$time" ] ; then \
		${ECHO_MSG} "===>  ERROR: could not read the commit date from the distfile." ; \
		exit 1 ; \
	fi ; \
	id=$$(${PRINTF} %s "$$commit" | cut -c1-12) ; \
	${ECHO_MSG} "===>  Stamping the build with commit $$id ($$time)" ; \
	${PRINTF} '_APIARY_BUILD_COMMIT=$$commit\n_APIARY_BUILD_ID=$$id\n_APIARY_BUILD_TIME=$$time\n' \
		> ${WRKDIR}/.buildinfo

GO_BUILDFLAGS+=	-ldflags "-X github.com/glenjbarber/apiary/internal/buildinfo.BuildID=${_APIARY_BUILD_ID} -X github.com/glenjbarber/apiary/internal/buildinfo.BuildTime=${_APIARY_BUILD_TIME} -X github.com/glenjbarber/apiary/internal/buildinfo.GitCommit=${_APIARY_BUILD_COMMIT}"

SUB_FILES=	README.md

# The build stamp's three values are read from the file pre-build wrote;
# see the comment on that target above for why they are recursive
# variables and why they are read there rather than here.
_APIARY_BUILD_COMMIT=	$$(sed -n 's/^_APIARY_BUILD_COMMIT=//p' ${WRKDIR}/.buildinfo)
_APIARY_BUILD_ID=	$$(sed -n 's/^_APIARY_BUILD_ID=//p' ${WRKDIR}/.buildinfo)
_APIARY_BUILD_TIME=	$$(sed -n 's/^_APIARY_BUILD_TIME=//p' ${WRKDIR}/.buildinfo)

# The runtime directories the package creates and owns. They are
# recorded with PLIST_DIRS so that `pkg info` shows them and `pkg
# delete` removes the ones that are empty.
PLIST_DIRS=	${_APIARY_DATADIR} \
		${_APIARY_DATADIR}/isos \
		${_APIARY_DATADIR}/raftd \
		${_APIARY_LOGDIR} \
		${_APIARY_RUNDIR}

PLIST_FILES=	"@sample ${_APIARY_CONFDIR}/frontend.json.sample" \
		"@sample ${_APIARY_CONFDIR}/managerd.json.sample" \
		"@sample ${_APIARY_CONFDIR}/raftd.json.sample" \
		${_APIARY_CONFDIR}/README.md \
		"@sample ${_APIARY_CONFDIR}/restshimd.json.sample" \
		${_APIARY_LIBEXECDIR}/apiary-mcp \
		${_APIARY_LIBEXECDIR}/apiaryinstall \
		${_APIARY_LIBEXECDIR}/frontend \
		${_APIARY_LIBEXECDIR}/managerd \
		${_APIARY_LIBEXECDIR}/raftd \
		${_APIARY_LIBEXECDIR}/restshimd \
		sbin/apiaryctl

PLIST_SUB=	_APIARY_CONFDIR=${_APIARY_CONFDIR} \
		_APIARY_DATADIR=${_APIARY_DATADIR} \
		_APIARY_LIBEXECDIR=${_APIARY_LIBEXECDIR} \
		_APIARY_LOGDIR=${_APIARY_LOGDIR} \
		_APIARY_RUNDIR=${_APIARY_RUNDIR}

# The packing list is expressed here rather than in a pkg-plist file.
# The two are equivalent, but pkg-plist cannot carry the %%VAR%%
# substitution the paths below need, and a hand-maintained pkg-plist is
# one more file that has to be updated by hand.
#
# The rc.d scripts are deliberately NOT here: USE_RC_SUBR installs them
# and adds them to the packing list itself.
#
# The sample configuration files are @sample, so that pkg upgrade will
# not overwrite an edited copy of one. They are installed by
# post-install, because they are named by the project's own
# etc/apiary/ directory rather than by anything the ports framework
# knows about.
# The upstream README is the port's documentation. It goes to
# ${PREFIX}/share/doc/apiary, which is where an operator looks for a
# package's documentation, rather than to the etc/apiary directory
# where the sample configuration files live.
PORTDOCS=	README.md

OPTIONS_DEFINE=		QCOW2
OPTIONS_DEFAULT=	QCOW2
QCOW2_DESC=		qcow2 disk image support (measure/expand via qemu-img)
QCOW2_RUN_DEPENDS=	qemu-img:emulators/qemu

post-install:
	@${MKDIR} ${STAGEDIR}${_APIARY_CONFDIR}
	@for s in README.md frontend.json.sample managerd.json.sample raftd.json.sample \
		restshimd.json.sample ; \
		do ${INSTALL_DATA} ${WRKSRC}/etc/apiary/$$s ${STAGEDIR}${_APIARY_CONFDIR}/$$s.tmp ; \
		${MV} ${STAGEDIR}${_APIARY_CONFDIR}/$$s.tmp \
		${STAGEDIR}${_APIARY_CONFDIR}/$$s ; \
		done
	@${MKDIR} ${STAGEDIR}${_APIARY_DATADIR}/raftd
	@${MKDIR} ${STAGEDIR}${_APIARY_DATADIR}/isos
	@${MKDIR} ${STAGEDIR}${_APIARY_RUNDIR}
	@${MKDIR} ${STAGEDIR}${_APIARY_LOGDIR}

.include <bsd.port.mk>
