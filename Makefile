# Makefile
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
	sudo mkdir -p /var/db/apiary/raftd /var/db/apiary/isos /var/run/apiary /var/log/apiary

setup-rcd:
	sudo cp etc/rc.d/apiary_* /usr/local/etc/rc.d/
	sudo chmod 555 /usr/local/etc/rc.d/apiary_*
	sudo sysrc apiary_raftd_enable=YES apiary_managerd_enable=YES apiary_frontend_enable=YES apiary_restshimd_enable=YES

setup-pam:
	test -f /etc/pam.d/${PAM_SERVICE} || \
		sudo sh -c "printf 'auth required pam_unix.so no_warn\\naccount required pam_unix.so\\n' > /etc/pam.d/${PAM_SERVICE}"
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
	sudo mkdir -p ${NODE_TLS_DIR}
	test -f ${NODE_TLS_DIR}/cert.pem -a -f ${NODE_TLS_DIR}/key.pem || \
		sudo openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
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
# the built binaries where the rc.d scripts expect them, and the
# apiary_managerd_args/apiary_frontend_args/
# apiary_restshimd_args a fresh node actually needs to be reachable at
# all - every one of these three daemons defaults its own listen
# address to 127.0.0.1 (loopback-only) when no arg is given at all, so
# skipping this step leaves every port genuinely unreachable from
# anywhere but the host itself, not merely unconfigured
# (-bhyve-bootrom is resolved here the same way Section 5 does by hand
# - the package only sometimes carries the real .fd file itself,
# edk2-bhyve usually carries it instead).
# NODE_ZFS_POOL/NODE_VLAN_UPLINK/NODE_BHYVE_BRIDGE/NODE_RPC_ADDR/
# NODE_HTTP_ADDR/NODE_REST_ADDR/NODE_TLS_DIR/PAM_SERVICE override the
# defaults for a host that doesn't match this one's layout, e.g.
# `make setup-quick NODE_VLAN_UPLINK=em0 NODE_HTTP_ADDR=10.62.0.2:8080`.
#
# Real login is enabled by default now: setup (a prerequisite, via
# setup-tls/setup-pam) already provisions both a TLS certificate and a
# PAM policy file unconditionally, so the one thing that used to block
# -pam-service (ADR-0087's TLS requirement) is always satisfied by the
# time this runs. frontend/restshimd both get -manager-tls plus
# -manager-tls-ca pointed at the same certificate, since a self-signed
# cert has no public CA to verify against otherwise - confirmed live,
# working through this exact chain of TLS failures one at a time
# (missing SAN, then unknown authority) before landing here.
# setup-admin (below) creates the actual UNIX account and prompts for
# its password interactively, so there's no separate manual step left
# before logging in - just start the services and log in as
# NODE_ADMIN_USER right away, since the first successful login on a
# Comb with no role map yet becomes Admin automatically (ADR-0086).
#
# The binary install loop copies to a .new name and mv's it into place
# rather than copying directly over the destination - found live, on a
# re-run against an already-running Comb: cp can't overwrite a binary
# a live process still has open ("Text file busy"), but mv is an
# atomic rename the running process's already-open file descriptor
# doesn't notice at all - the same fix this project's own live
# apiarium/apiverse deploys already use for exactly this reason.
setup-quick:
	pkg install -y go git sudo
	${MAKE} build
	${MAKE} setup
	${MAKE} setup-admin
	./apiaryinstall -apply -apply-network yes-modify-network -zfs-pool ${NODE_ZFS_POOL} \
		-vlan-uplink ${NODE_VLAN_UPLINK} -bhyve-bridge ${NODE_BHYVE_BRIDGE}
	sudo mkdir -p /usr/local/libexec/apiary
	for S in ${SRCS} ; \
		do sudo cp -p $$S /usr/local/libexec/apiary/$$S.new ;\
		sudo chmod +x /usr/local/libexec/apiary/$$S.new ;\
		sudo mv /usr/local/libexec/apiary/$$S.new /usr/local/libexec/apiary/$$S ;\
	done
	BOOTROM=$$(test -f /usr/local/share/uefi-firmware/BHYVE_UEFI.fd && echo /usr/local/share/uefi-firmware/BHYVE_UEFI.fd || pkg info -l edk2-bhyve 2>/dev/null | grep '\.fd$$' | head -1) ;\
	test -n "$$BOOTROM" || { echo "could not locate a bhyve UEFI firmware .fd file - install bhyve-firmware/edk2-bhyve and re-run" >&2 ; exit 1 ; } ;\
	sudo sysrc apiary_managerd_args="-rpc-addr ${NODE_RPC_ADDR} -bhyve-bootrom $$BOOTROM -bhyve-bridge ${NODE_BHYVE_BRIDGE} -vlan-uplink ${NODE_VLAN_UPLINK} -tls-cert ${NODE_TLS_DIR}/cert.pem -tls-key ${NODE_TLS_DIR}/key.pem -pam-service ${PAM_SERVICE}"
	sudo sysrc apiary_frontend_args="-http-addr ${NODE_HTTP_ADDR} -manager-tls -manager-tls-ca ${NODE_TLS_DIR}/cert.pem"
	sudo sysrc apiary_restshimd_args="-http-addr ${NODE_REST_ADDR} -manager-tls -manager-tls-ca ${NODE_TLS_DIR}/cert.pem"
	@echo "apiary_managerd_args/apiary_frontend_args/apiary_restshimd_args set, including real login (-pam-service ${PAM_SERVICE}). Start the services (service apiary_raftd start && service apiary_managerd start && service apiary_frontend start && service apiary_restshimd start), then log in as ${NODE_ADMIN_USER} right away: whoever logs in first on a Comb with no role map yet becomes Admin automatically (ADR-0086)."

NODE_ADMIN_USER?=	admin
NODE_ROLE_MAP?=		/var/db/apiary/frontend-role-map.json

# setup-admin creates NODE_ADMIN_USER and prompts for its password
# interactively via passwd(1) - a single-node bring-up's whole point is
# to end with one working login, not a manual pw useradd/passwd
# afterward. Only runs useradd/passwd when the account doesn't already
# exist: re-running setup-quick (or setup-admin directly) must never
# silently reset an existing admin's password out from under them, the
# same "only act if absent" contract setup-pam/setup-tls already
# follow. To rotate an existing account's password instead, run
# `passwd ${NODE_ADMIN_USER}` directly.
#
# Checks NODE_ROLE_MAP's own contents before doing anything else - a
# real gap found live: the first version of this target only checked
# whether the UNIX account already existed, so it happily created one
# and set its password even when ADR-0086's own bootstrap condition
# (the role map genuinely empty) no longer held, leaving a real
# account whose first login could never become Admin - "no Apiary role
# is assigned to this account" instead. The role map, once it exists
# at all, is a small `{"role_map": {...}}` file written by
# applyRoleMapLocked (internal/frontend/server.go); stripping
# whitespace before matching handles both its own pretty-printed form
# and a hand-edited compact one identically.
setup-admin:
	if [ -f ${NODE_ROLE_MAP} ] && ! tr -d ' \t\n' < ${NODE_ROLE_MAP} | grep -qE '"role_map":(\{\}|null)' ; then \
		echo "${NODE_ROLE_MAP} already has role entries - a new ${NODE_ADMIN_USER} login would NOT automatically become Admin (ADR-0086 only bootstraps while the role map is genuinely empty). Skipping account setup - grant a role through /users as an existing Admin instead." >&2 ; \
		exit 0 ; \
	fi ; \
	pw usershow ${NODE_ADMIN_USER} >/dev/null 2>&1 && \
		echo "user ${NODE_ADMIN_USER} already exists - not touching its password (run passwd ${NODE_ADMIN_USER} directly to change it)" || \
		{ sudo pw useradd -n ${NODE_ADMIN_USER} -m -s /bin/sh && sudo passwd ${NODE_ADMIN_USER} ; }
