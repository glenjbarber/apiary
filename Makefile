# Makefile
#
# Every target that touches the filesystem outside this checkout
# (/usr/local, /var/db, /var/run, /var/log, /etc) assumes it is being
# run with root privileges already - as root directly, or via `sudo
# make <target>` wrapping the whole invocation. No individual recipe
# line calls `sudo` itself: doing so per-line only matters for a
# non-root, non-sudo'd `make` invocation, which was never a supported
# case (an Apiary Comb's admin is assumed to actually have admin
# access on the host), and it multiplies password prompts for no
# benefit when the whole invocation is already privileged.
SRCS=		apiaryinstall \
			raftd \
			managerd \
			frontend \
			restshimd

PAM_SERVICE=	apiary

build:
	for S in ${SRCS} ; \
		do go build -buildvcs=false -o $$S ./cmd/$$S ;\
	done

clean:
	for S in ${SRCS} ; \
		do rm -f $$S ; \
	done

INSTALL_SRCS=	raftd \
		managerd \
		frontend \
		restshimd

# install copies the four apiary daemons - not apiaryinstall, a
# one-shot host-prep CLI meant to be run from this checkout and never
# installed permanently - to the fixed path every etc/rc.d/apiary_*
# script execs, /usr/local/libexec/apiary/<name> (see docs/bootstrap.md
# Step 6 for the manual equivalent this mirrors exactly). Depends on
# setup-dirs so /var/log/apiary, /var/run/apiary, etc. already exist -
# a first `service apiary_raftd start` on a truly fresh host fails
# outright without /var/log/apiary in particular. Copies to a .new name
# and atomically renames into place rather than overwriting the
# destination directly: cp can't overwrite a binary a live process
# still has open ("Text file busy"), but mv's atomic rename doesn't
# disturb the running process's already-open file descriptor at all -
# the same fix this project's own live apiarium/apiverse deploys use.
# Re-run this after every rebuild, then `service apiary_<name> restart`.
#
# Also installs each daemon's commented .json.sample reference file
# (etc/apiary/<name>.json.sample) alongside its real config path, e.g.
# /usr/local/etc/apiary/raftd.json.sample next to raftd.json. These are
# pure documentation, never read by any daemon (JSON has no comment
# syntax, and every daemon parses its config with plain encoding/json -
# see docs/bootstrap.md Steps 7-9), so they are always refreshed on
# every install, unlike the real config files they sit beside, which
# this target never touches. Copy one to the real path and strip its
# "//" lines to use it (each is written so every comment stands on its
# own line and never trails a value, so a plain `grep -v '^\s*//'`
# does this safely).
install: build setup-dirs
	mkdir -p /usr/local/libexec/apiary /usr/local/etc/apiary
	for S in ${INSTALL_SRCS} ; \
		do cp -p $$S /usr/local/libexec/apiary/$$S.new ;\
		chmod +x /usr/local/libexec/apiary/$$S.new ;\
		mv /usr/local/libexec/apiary/$$S.new /usr/local/libexec/apiary/$$S ;\
	done
	for S in ${INSTALL_SRCS} ; \
		do cp -p etc/apiary/$$S.json.sample /usr/local/etc/apiary/$$S.json.sample ;\
		chmod 644 /usr/local/etc/apiary/$$S.json.sample ;\
	done

# setup installs the pieces a fresh host needs beyond the built binaries
# themselves: the log/run/data directories every apiary_* rc.d script or
# daemon expects to already exist, the rc.d scripts (etc/rc.d/apiary_*),
# a PAM policy file for real login, and a TLS certificate for
# managerd's external API (required before -pam-service is even
# accepted - ADR-0087) - see docs/bootstrap.md's Step 11 for the full
# manual walkthrough this mirrors, including why the PAM file is
# written with printf rather than a pasted heredoc (tab corruption).
# Safe to re-run: the directories/rc.d scripts/sysrc enables are always
# refreshed to match this checkout, but the PAM file and TLS cert are
# only written if absent, so a later hand-edited /etc/pam.d/apiary or a
# real (non-self-signed) certificate dropped in NODE_TLS_DIR is never
# clobbered by a re-run.
setup: setup-dirs setup-rcd setup-pam setup-tls

# setup-dirs mirrors docs/bootstrap.md's own manual `mkdir -p` step.
# /var/log/apiary is the one genuine gap: every apiary_* rc.d script's
# daemon(8) invocation opens its logfile there immediately on start,
# before the daemon binary itself ever runs, so nothing in the Go code
# can create it lazily the way raftd's own socket dir (/var/run/apiary)
# or isostore's data dir (/var/db/apiary/isos) already do at their own
# startup - a first `service apiary_raftd start` on a truly fresh host
# fails outright without this. Created here anyway, alongside the
# others, so one target covers every directory bootstrap.md's manual
# step lists rather than leaving an operator to remember which
# directories are and aren't self-creating.
setup-dirs:
	mkdir -p /var/db/apiary/raftd /var/db/apiary/isos /var/run/apiary /var/log/apiary

setup-rcd:
	cp etc/rc.d/apiary_* /usr/local/etc/rc.d/
	chmod 555 /usr/local/etc/rc.d/apiary_*
	sysrc apiary_raftd_enable=YES apiary_managerd_enable=YES apiary_frontend_enable=YES apiary_restshimd_enable=YES

setup-pam:
	test -f /etc/pam.d/${PAM_SERVICE} || \
		printf 'auth required pam_unix.so no_warn\naccount required pam_unix.so\n' > /etc/pam.d/${PAM_SERVICE}
	@echo "PAM policy at /etc/pam.d/${PAM_SERVICE} - pass -pam-service ${PAM_SERVICE} to managerd to enable real login; the first successful login becomes Admin automatically (see docs/bootstrap.md Step 11)."

NODE_TLS_DIR?=	/usr/local/etc/apiary-tls

# setup-tls generates a self-signed certificate for managerd's external
# API if NODE_TLS_DIR doesn't already have one - only written if
# absent, so a real (CA-issued) certificate placed there instead is
# never clobbered by a re-run. Must carry a Subject Alternative Name,
# not just a CN: Go's TLS client has ignored CN-only certs for
# hostname verification since 1.15, and every daemon that dials
# managerd (frontend/restshimd) defaults to 127.0.0.1, so that IP is
# the one SAN that actually matters for the common single-node case -
# confirmed live: a CN-only cert, and separately a cert missing this
# specific IP SAN, both failed real verification with distinct,
# individually-confusing x509 errors before this was added.
setup-tls:
	mkdir -p ${NODE_TLS_DIR}
	test -f ${NODE_TLS_DIR}/cert.pem -a -f ${NODE_TLS_DIR}/key.pem || \
		openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
			-keyout ${NODE_TLS_DIR}/key.pem -out ${NODE_TLS_DIR}/cert.pem \
			-subj "/CN=$$(hostname)" \
			-addext "subjectAltName=IP:127.0.0.1,DNS:$$(hostname)"
	@echo "TLS cert at ${NODE_TLS_DIR}/cert.pem (self-signed, only written if absent - drop a real certificate/key at this path before running setup to use one instead)."

NODE_ZFS_POOL?=		zroot
NODE_VLAN_UPLINK?=	vtnet0
NODE_BHYVE_BRIDGE?=	bridge0
NODE_RPC_ADDR?=		0.0.0.0:17700
NODE_HTTP_ADDR?=	0.0.0.0:8080
NODE_REST_ADDR?=	0.0.0.0:8081

# setup-quick is the whole docs/bootstrap.md preflight sequence
# (Sections 2-9) collapsed into one target for a single-node bring-up
# on a genuinely fresh checkout - no assumption that `make build` (or
# anything else) already ran: packages, building every binary, running
# apiaryinstall's safe fixes plus its one risky network step, installing
# the built binaries where the rc.d scripts expect them, and writing
# each daemon's real /usr/local/etc/apiary/<name>.json config directly
# (ADR-0100 - CLI flags/sysrc *_args are legacy, not used here or
# anywhere else in this Makefile) - every one of raftd/managerd/
# frontend/restshimd defaults its own listen address to 127.0.0.1
# (loopback-only) with no config at all, so skipping this step leaves
# every port genuinely unreachable from anywhere but the host itself,
# not merely unconfigured (-bhyve-bootrom is resolved here the same
# way Section 5 does by hand - the package only sometimes carries the
# real .fd file itself, edk2-bhyve usually carries it instead).
# NODE_ZFS_POOL/NODE_VLAN_UPLINK/NODE_BHYVE_BRIDGE/NODE_RPC_ADDR/
# NODE_HTTP_ADDR/NODE_REST_ADDR/NODE_TLS_DIR/PAM_SERVICE override the
# defaults for a host that doesn't match this one's layout, e.g.
# `make setup-quick NODE_VLAN_UPLINK=em0 NODE_HTTP_ADDR=10.62.0.2:8080`.
# node_id uses this host's own hostname, never the doc's own
# "<this-node-id>" placeholder text - a real handoff bug (see
# SHARED.md) came from that literal placeholder getting copied verbatim
# and committing itself into persistent raft state.
#
# Real login and TLS are both mandatory here, not optional: setup (a
# prerequisite, via setup-tls/setup-pam) already provisions both a TLS
# certificate and a PAM policy file unconditionally, so managerd.json
# below always sets tls_cert/tls_key/pam_service together, and
# frontend.json/restshimd.json always set manager_tls/manager_tls_ca to
# match, even for a pure loopback, single-node Comb - confirmed live,
# working through this exact chain of TLS failures one at a time
# (missing SAN, then unknown authority) before landing on this shape.
#
# No account-creation step here (removed - see SHARED.md's own dated
# entry for why): PAM only needs some valid UNIX account with a
# password, not specifically one Apiary creates. Whichever account is
# already running this `make setup-quick` invocation (or any other
# existing account) becomes Admin automatically on its first
# successful login, since the role map on a fresh Comb starts out
# genuinely empty (ADR-0086) - just start the services and log in.
#
# The binary-install step (${MAKE} install, plus a separate copy for
# apiaryinstall alone below - see the `install` target's own comment
# for why apiaryinstall isn't part of `install` itself) uses the same
# .new-then-mv atomic rename this project's own live apiarium/apiverse
# deploys already rely on, so re-running setup-quick against an
# already-running Comb never hits "Text file busy". The config files
# below are written unconditionally on every run, matching a fresh
# checkout's values - re-running this against a Comb with hand-edited
# config will overwrite those edits (unlike setup-pam/setup-tls's own
# "only if absent" contract, since these carry real per-host settings
# that only setup-quick itself knows how to regenerate correctly).
setup-quick:
	pkg install -y go git
	${MAKE} build
	${MAKE} setup
	./apiaryinstall -apply -apply-network yes-modify-network -zfs-pool ${NODE_ZFS_POOL} \
		-vlan-uplink ${NODE_VLAN_UPLINK} -bhyve-bridge ${NODE_BHYVE_BRIDGE}
	${MAKE} install
	cp -p apiaryinstall /usr/local/libexec/apiary/apiaryinstall.new
	chmod +x /usr/local/libexec/apiary/apiaryinstall.new
	mv /usr/local/libexec/apiary/apiaryinstall.new /usr/local/libexec/apiary/apiaryinstall
	BOOTROM=$$(test -f /usr/local/share/uefi-firmware/BHYVE_UEFI.fd && echo /usr/local/share/uefi-firmware/BHYVE_UEFI.fd || pkg info -l edk2-bhyve 2>/dev/null | grep '\.fd$$' | head -1) ;\
	test -n "$$BOOTROM" || { echo "could not locate a bhyve UEFI firmware .fd file - install bhyve-firmware/edk2-bhyve and re-run" >&2 ; exit 1 ; } ;\
	NODEID=$$(hostname) ;\
	RPCADDR="${NODE_RPC_ADDR}" ;\
	RPCPORT=$${RPCADDR##*:} ;\
	printf '{\n  "data_dir": "/var/db/apiary/raftd",\n  "socket": "/var/run/apiary/raftd.sock",\n  "node_id": "%s"\n}\n' "$$NODEID" > /usr/local/etc/apiary/raftd.json ;\
	chmod 600 /usr/local/etc/apiary/raftd.json ;\
	printf '{\n  "raftd_socket": "/var/run/apiary/raftd.sock",\n  "rpc_addr": "%s",\n  "node_id": "%s",\n  "zfs_base": "%s/apiary",\n  "bhyve_bootrom": "%s",\n  "bhyve_bridge": "%s",\n  "uplink": "%s",\n  "iso_dir": "/var/db/apiary/isos",\n  "tls_cert": "%s/cert.pem",\n  "tls_key": "%s/key.pem",\n  "pam_service": "%s"\n}\n' "${NODE_RPC_ADDR}" "$$NODEID" "${NODE_ZFS_POOL}" "$$BOOTROM" "${NODE_BHYVE_BRIDGE}" "${NODE_VLAN_UPLINK}" "${NODE_TLS_DIR}" "${NODE_TLS_DIR}" "${PAM_SERVICE}" > /usr/local/etc/apiary/managerd.json ;\
	chmod 600 /usr/local/etc/apiary/managerd.json ;\
	printf '{\n  "manager_addr": "127.0.0.1:%s",\n  "http_addr": "%s",\n  "manager_tls": true,\n  "manager_tls_ca": "%s/cert.pem"\n}\n' "$$RPCPORT" "${NODE_HTTP_ADDR}" "${NODE_TLS_DIR}" > /usr/local/etc/apiary/frontend.json ;\
	printf '{\n  "manager_addr": "127.0.0.1:%s",\n  "http_addr": "%s",\n  "manager_tls": true,\n  "manager_tls_ca": "%s/cert.pem"\n}\n' "$$RPCPORT" "${NODE_REST_ADDR}" "${NODE_TLS_DIR}" > /usr/local/etc/apiary/restshimd.json
	@echo "/usr/local/etc/apiary/{raftd,managerd,frontend,restshimd}.json written, including real login (pam_service=${PAM_SERVICE}) and TLS. Start the services (service apiary_raftd start && service apiary_managerd start && service apiary_frontend start && service apiary_restshimd start), then log in with any existing UNIX account right away: whoever logs in first on a Comb with no role map yet becomes Admin automatically (ADR-0086)."
