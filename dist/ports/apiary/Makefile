# $FreeBSD$

PORTNAME=	apiary
DISTVERSION=	1.0.0.a0
PORTREVISION=	0
CATEGORIES=	sysutils

GH_ACCOUNT=	glenjbarber
GH_PROJECT=	apiary
USE_GITHUB=	yes
DISTVERSIONPREFIX=	v

GO_MOD_DIST=	github
GO_MODULE=	github.com/glenjbarber/apiary

MAINTAINER=	gjb@FreeBSD.org
COMMENT=	Distributed virtualisation control plane for FreeBSD Combs
WWW=		https://apiary.work

LICENSE=	BSD2CLAUSE
LICENSE_FILE=	${WRKSRC}/LICENSE

USES=		go:modules

RUN_DEPENDS=	dns/dnsmasq:dnsmasq

OPTIONS_DEFINE=	QCOW2
OPTIONS_DEFAULT=	QCOW2
QCOW2_DESC=	qcow2 disk image support (measure/expand via qemu-img)
QCOW2_RUN_DEPENDS=	emulators/qemu:qemu

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

APIARY_RCSCRIPTS=	apiary_raftd \
			apiary_managerd \
			apiary_frontend \
			apiary_restshimd \
			apiary_mcp

GO_TARGET=	./cmd/raftd:${APIARY_LIBEXECDIR}/raftd \
		./cmd/managerd:${APIARY_LIBEXECDIR}/managerd \
		./cmd/frontend:${APIARY_LIBEXECDIR}/frontend \
		./cmd/restshimd:${APIARY_LIBEXECDIR}/restshimd \
		./cmd/apiaryctl:${APIARY_SBINDIR}/apiaryctl \
		./cmd/apiaryinstall:${APIARY_LIBEXECDIR}/apiaryinstall \
		./cmd/apiary-mcp:${APIARY_LIBEXECDIR}/apiary-mcp

GO_BUILDFLAGS+=	-ldflags "-X github.com/glenjbarber/apiary/internal/buildinfo.BuildID=${APIARY_BUILD_ID} -X github.com/glenjbarber/apiary/internal/buildinfo.BuildTime=${APIARY_BUILD_TIME} -X github.com/glenjbarber/apiary/internal/buildinfo.GitCommit=${APIARY_BUILD_COMMIT}"

APIARY_BUILD_COMMIT=	0000000000000000000000000000000000000000
APIARY_BUILD_ID=	000000000000
APIARY_BUILD_TIME=	1970-01-01T00:00:00Z

PLIST_SUB=	APIARY_LIBEXECDIR=${APIARY_LIBEXECDIR} \
		APIARY_CONFDIR=${APIARY_CONFDIR} \
		APIARY_DATADIR=${APIARY_DATADIR} \
		APIARY_LOGDIR=${APIARY_LOGDIR} \
		APIARY_RUNDIR=${APIARY_RUNDIR}

SUB_FILES=	README.md

post-extract:
	@if [ "${APIARY_BUILD_COMMIT}" = "0000000000000000000000000000000000000000" ]; then \
		${ECHO_MSG} "===>  ERROR: APIARY_BUILD_COMMIT is still the placeholder." ; \
		${ECHO_MSG} "    Check out the release tag, run 'git rev-parse HEAD'," ; \
		${ECHO_MSG} "    and put the commit and its date in this Makefile." ; \
		exit 1 ; \
	fi

post-install:
	@${MKDIR} ${STAGEDIR}${PREFIX}/etc/rc.d
	@for s in ${APIARY_RCSCRIPTS} ; do \
		${INSTALL_SCRIPT} ${WRKSRC}/etc/rc.d/$$s ${STAGEDIR}${PREFIX}/etc/rc.d ; \
	done
	@${MKDIR} ${STAGEDIR}${APIARY_CONFDIR}
	@for s in README.md frontend.json.sample managerd.json.sample \
		raftd.json.sample restshimd.json.sample ; do \
		${INSTALL_DATA} ${WRKSRC}/etc/apiary/$$s ${STAGEDIR}${APIARY_CONFDIR}/$$s.tmp ; \
		${MV} ${STAGEDIR}${APIARY_CONFDIR}/$$s.tmp ${STAGEDIR}${APIARY_CONFDIR}/$$s ; \
	done
	@${MKDIR} ${STAGEDIR}${APIARY_DATADIR}/raftd
	@${MKDIR} ${STAGEDIR}${APIARY_DATADIR}/isos
	@${MKDIR} ${STAGEDIR}${APIARY_RUNDIR}
	@${MKDIR} ${STAGEDIR}${APIARY_LOGDIR}
.include <bsd.port.mk>
