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

# BUILD_ID is the build's identity, and it is computed by
# scripts/build-ldflags.sh rather than here. Override on the command
# line to pin a build to a known id (e.g. a release); empty means "work
# it out from the checkout". Either spelling works:
#
#	make build BUILD_ID=release-1.0
#	BUILD_ID=release-1.0 make build
#
# The id is the commit and nothing else. There is deliberately no clock
# in it, because the two properties an operator needs are in tension
# and only one of them is real: a timestamp makes every rebuild look
# like a different build even when the bytes are identical, while the
# commit hash is a property of the source that holds forever. So the
# hash goes in and the clock does not, and the same commit built twice
# is the same build twice - which is what makes an artifact's sha256 a
# checkable fact rather than a curiosity.
#
# A dirty worktree is the one case the commit cannot describe, because
# the binary then is not the commit. It gets an explicit -dirty suffix
# instead of a timestamp: -dirty says precisely what is true (the bytes
# are not described by the commit) without claiming a new identity every
# time the tree is touched. Untracked files count - an untracked file
# compiles into the binary just as a modified one does.
#
# The consequence to be aware of: a -dirty build is NOT reproducible and
# two of them can share an id while differing in bytes. That is why
# buildinfo and versioncheck both report the -dirty case as weaker
# evidence rather than as agreement.
BUILD_ID?=	# empty: computed by scripts/build-ldflags.sh
BUILD_TIME?=	# empty: the commit's date, likewise computed there

BUILD_PKG=	github.com/glenjbarber/apiary/internal/buildinfo
LDFLAGS_SH=	scripts/build-ldflags.sh
VERDICT_SH=	scripts/stamp-verdict.sh

# -trimpath strips the build directory and module cache paths out of the
# binary. Without it the same commit built in two checkouts produces
# different bytes, so reproducibility would hold only for whoever
# happened to build it first. -buildvcs=false because the Makefile
# injects the revision explicitly and does not want Go stamping a dirty
# tree's revision as though it described the binary.
GO_BUILD?=	go build -trimpath -buildvcs=false

# Check a binary's identity without starting it. A copy over a running
# executable leaves new bytes on disk and old ones in memory, so this
# is the only way to tell a deployed build from a running one.
.PHONY: version
version: build
	@for S in ${SRCS} ; \
		do printf '%-16s ' "$$S" ; ./$$S -version 2>&1 | head -1 ;\
	done

PAM_SERVICE=	apiary

build:
	set -e; \
	for S in ${SRCS} ; \
		do ldflags=`BUILD_ID='$(BUILD_ID)' BUILD_TIME='$(BUILD_TIME)' ${LDFLAGS_SH}` ; \
		$(GO_BUILD) -ldflags "$$ldflags" -o $$S ./cmd/$$S ;\
	done

# The stamp script is load-bearing for identity, and check-stamped only
# notices a broken one after a full build of five binaries. This checks
# its contract directly, and is the one that would have caught the BSD
# make $(shell) problem in a second rather than after a deploy.
.PHONY: check-ldflags
check-ldflags:
	@set -e; \
	out=`${LDFLAGS_SH}` ; \
	echo "raw: $$out" ; \
	for V in BuildID BuildTime GitCommit ; do \
		val=`echo "$$out" | tr ' ' '\n' | sed -n "s/.*\.$$V=//p"` ; \
		if [ -z "$$val" ] ; then \
			echo "$$V is empty or missing in: $$out" >&2 ; exit 1 ; \
		fi ; \
		echo "  $$V=$$val" ; \
	done ; \
	id=`${LDFLAGS_SH} --id` ; \
	short=`git rev-parse --short=12 HEAD 2>/dev/null || echo nogit` ; \
	case "$$id" in "$$short"|"$$short-dirty") ;; \
		*) echo "id $$id does not derive from commit $$short" >&2 ; exit 1 ;; \
	esac ; \
	echo "ldflags ok"

# Refuse to let a binary that cannot name itself reach
# /usr/local/libexec. This exists because a stamp that silently fails to
# apply is worse than no stamp: the build succeeds, the binary is
# installed, and every daemon reports build=unknown while the whole
# toolchain looks like it is working. That is not hypothetical - it is
# what BSD make's lack of $(shell) did to every build on the testbed.
# The refusal is what turns the next occurrence into a failed `make
# install` instead of another silent bad deploy.
#
# A -dirty build is NOT that, and refusing it was a second mistake with
# a worse failure mode than the one it was fixing. A dirty binary says
# exactly what is true: here is a commit, and here is the fact that the
# worktree was not clean, so the commit does not describe these bytes.
# That is more information than a clean build carries, not less. But it
# is also completely ordinary - a scratch file in the checkout is enough
# - so refusing it blocked routine deploys, and the only way through was
# ALLOW_UNIDENTIFIED=1, which ALSO suppresses the unidentifiable case.
# The sole remedy for a too-strict check was therefore to disable the
# check that matters, which is how a guard gets disabled. A dirty build
# now warns loudly and installs, and ALLOW_UNIDENTIFIED=1 means what it
# says: accept a binary whose identity cannot be verified at all.
#
# scripts/stamp-verdict.sh owns the ok/dirty/fail decision so the
# classification has one home instead of a case statement in a recipe
# that a verification script has to re-implement in order to test it.
#
# The recipe's final case arm catches a verdict that is not one of the
# three at all - the script missing, unreadable or broken - and refuses.
# An unclassifiable id is an unusable id, and an if/elif chain with a
# trailing else would have reported it as ok, which is the one result
# that must never come from a broken guard.
#
# No # comments inside this recipe: a backslash continuation joins every
# line into one, and a # then comments out the rest of it, silently
# deleting the commands after it. It cost one wrong turn to find.
#
# Only a prerequisite of install, not of build, because it has to RUN
# the binaries: a binary cross-compiled for FreeBSD on a Mac cannot
# answer -version here, and failing that would break the cross-build
# path rather than protect anything.
.PHONY: check-stamped
check-stamped:
	@fail=0 ; dirty="" ; \
	for S in ${SRCS} ; do \
		id=`./$$S -version 2>&1 | grep -o 'build=[^ ]*' | head -1 | cut -d= -f2` ; \
		verdict=`${VERDICT_SH} "$$id"` ; \
		class=`echo "$$verdict" | cut -d' ' -f1` ; \
		reason=`echo "$$verdict" | sed 's/^[^ ]* //'` ; \
		case "$$class" in \
		fail) \
			echo "$$S: REFUSING - $$reason" >&2 ; fail=1 ;; \
		dirty) \
			dirty="$$dirty $$S=$$id" ; \
			echo "$$S: WARNING - $$reason" >&2 ;; \
		ok) \
			echo "$$S: $$reason" ;; \
		*) \
			echo "$$S: REFUSING - $$id" >&2 ; \
			echo "  ${VERDICT_SH} gave no verdict for it, so the stamp" >&2 ; \
			echo "  cannot be trusted. Run 'make check-ldflags'." >&2 ; \
			fail=1 ;; \
		esac ; \
	done ; \
	if [ -n "$$dirty" ] ; then \
		echo "" >&2 ; \
		echo "WARNING:$$dirty" >&2 ; \
		echo "  Installed anyway. These binaries know their commit and know the" >&2 ; \
		echo "  worktree was not clean; what they cannot do is tell you which" >&2 ; \
		echo "  bytes they are, because two builds from two dirty worktrees" >&2 ; \
		echo "  share an id. Only a hash compares artifacts:" >&2 ; \
		printf '    sha256 %s\n' "$$(echo ${SRCS})" >&2 ; \
		echo "    sha256 /usr/local/libexec/apiary/*" >&2 ; \
		echo "  This worktree is not clean because of:" >&2 ; \
		${LDFLAGS_SH} --why 2>&1 | sed -n '1,10s/^/    /p' >&2 ; \
	fi ; \
	if [ $$fail -ne 0 ] ; then \
		if [ -n "$$ALLOW_UNIDENTIFIED" ] ; then \
			echo "ALLOW_UNIDENTIFIED is set - installing anyway." >&2 ; \
		else \
			echo "Refusing to install a binary that cannot identify itself." >&2 ; \
			echo "Fix the build, or set ALLOW_UNIDENTIFIED=1 to accept it." >&2 ; \
			echo "(That override is for a binary with no usable id. A -dirty" >&2 ; \
			echo "build is not one of those and never needed it.)" >&2 ; \
			exit 1 ; \
		fi ; \
	fi

# Prove the reproducibility claim instead of asserting it: build the
# same commit twice and compare the bytes. This is the check that fails
# if anyone reintroduces a clock, or drops -trimpath, or adds a stamp
# that varies per build - all of which are invisible in code review and
# only show up here.
.PHONY: check-reproducible
check-reproducible:
	@ldflags=`BUILD_ID='$(BUILD_ID)' BUILD_TIME='$(BUILD_TIME)' ${LDFLAGS_SH}` ; \
	$(GO_BUILD) -ldflags "$$ldflags" -o /tmp/apiary-repro-a ./cmd/raftd ; \
	sleep 1 ; \
	$(GO_BUILD) -ldflags "$$ldflags" -o /tmp/apiary-repro-b ./cmd/raftd ; \
	a=`sha256 -q /tmp/apiary-repro-a` ; b=`sha256 -q /tmp/apiary-repro-b` ; \
	rm -f /tmp/apiary-repro-a /tmp/apiary-repro-b ; \
	if [ "$$a" = "$$b" ] ; then \
		echo "reproducible: raftd $$a" ; \
	else \
		echo "NOT reproducible: $$a != $$b" ; exit 1 ; \
	fi
	@echo "build id was: `BUILD_ID='$(BUILD_ID)' BUILD_TIME='$(BUILD_TIME)' ${LDFLAGS_SH} --id`"

clean:
	for S in ${SRCS} ; \
		do rm -f $$S ; \
	done

INSTALL_SRCS=	raftd \
		managerd \
		frontend \
		restshimd

# `update` and `force-restart` between them restart every daemon `install`
# puts on disk, and they deliberately do not overlap:
#
#   update          frontend, restshimd
#   force-restart   managerd, raftd
#
# The split is the design, not a convenience. `update` is the target an
# operator (or a deploy loop) runs across every Comb without thinking, so
# nothing in it may be able to cost the colony its quorum. Everything that
# can is in `force-restart`, which is named for what it does to the
# guardrail: it restarts a daemon by handing `service` a restart directly,
# with no lease, no ADR-0125 quorum preflight and no cross-node
# coordination, because this Makefile has no client that speaks the guarded
# RPCs. The Machine page's per-service control (ADR-0125), or `apiaryctl`
# (ADR-0136), remains the path that does coordinate. This is the named
# escape hatch, deliberately per-Comb, not the recommended default.
#
# raftd is in `force-restart` and out of `update` for the reason ADR-0125
# records: `update` across all four Combs is the obvious thing to do after
# a deploy, and restarting all four raft voters at once costs the cluster
# its quorum. Keeping raftd out of `update` means that mistake is no
# longer available to make by accident. Bringing one Comb's raftd onto a
# new build is now a named, per-Comb act. It is not forbidden - an
# operator who knows they are on one Comb and mean to restart it can and
# should - but it is no longer something a deploy does behind you.
#
# The cost is real and worth stating plainly: `update` still installs
# raftd's binary and leaves the process alone, so raftd can go stale
# relative to every other daemon on the box. That is not hypothetical, it
# is the shape of the bug this split was built after: three of four raftd
# processes sat on a build days older than the rest, while the on-disk
# binary's mtime claimed the node was deployed. `cmd/versioncheck` is
# what tells a running process apart from the binary sitting beside it.
UPDATE_RESTART_SRCS=	frontend \
			restshimd

# Order is load-bearing: managerd first, raftd second. raftd's
# confirm-on-startup hook dials managerd over TLS to release the restart
# lease it is holding, on a bounded 5-attempt x 3s budget, and a lease has
# no TTL. Burning that budget against a managerd that is not up yet does
# not degrade the node - it blocks the cluster until an operator forces
# the lease clear, which is the exact failure the guardrail exists to
# prevent. Restarting raftd while managerd is down is the one way this
# target could cause it.
#
# The reverse order carries no such risk: managerd tolerates raftd being
# unavailable and reconnects, so the safe order is also the natural one.
FORCE_RESTART_SRCS=	managerd \
			raftd

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
# Also installs each daemon's .json.sample reference file
# (etc/apiary/<name>.json.sample) alongside its real config path, e.g.
# /usr/local/etc/apiary/raftd.json.sample next to raftd.json, plus
# etc/apiary/README.md, which is where the guidance those samples used
# to carry inline now lives. The samples are pure documentation, never
# read by any daemon, and are valid strict JSON exactly as installed -
# an operator can `cp` one straight onto its real path and have the
# daemon parse it, with no editing step (they carry <placeholder> values
# to substitute; see the README). Two of the loaders, frontend's and
# restshimd's, parse through internal/jsonstrict, which rejects
# duplicate object keys as well as comments, so a sample that
# round-tripped under plain encoding/json is not automatically valid
# for them either - internal/configsamples tests every shipped sample
# through the loader that will actually read it.
#
# Because they are documentation they are always refreshed on every
# install, unlike the real config files they sit beside, which this
# target never touches. The README must be installed too: the samples
# are now bare JSON, so an operator on a node with no README has no
# access to the SAN requirements, the bind-versus-dial rule, or
# restshimd's loopback posture.
install: build check-stamped setup-dirs
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
	cp -p etc/apiary/README.md /usr/local/etc/apiary/README.md ;\
	chmod 644 /usr/local/etc/apiary/README.md

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
	@echo "PAM policy at /etc/pam.d/${PAM_SERVICE} - set pam_service to ${PAM_SERVICE} in managerd.json, then restart apiary_managerd followed by apiary_frontend; the first successful login becomes Admin automatically (see docs/bootstrap.md Step 11)."

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

# NODE_RPC_ADDR is managerd's own BIND address (net.Listen), not a
# destination, and the two roles take different values - see
# internal/addrpolicy for the rule this default exists to stop
# breaking. It defaults to loopback because that is cmd/managerd's own
# startup default, so a single-node setup-quick writes exactly the value
# managerd would have used with no config file at all.
#
# ON A MULTI-COMB COLONY YOU MUST OVERRIDE IT WITH THIS NODE'S OWN
# RESOLVABLE NAME, whose DNS SAN its certificate carries:
#
#	make setup-quick NODE_RPC_ADDR=brood.lab3.home.arpa:17700
#
# A comb's peers - and its own raftd confirm hook - have to reach managerd
# over TLS, and Go verifies whatever host was dialed against that
# certificate's SANs. Apiary-issued serving certificates carry
# DNS:<node>.<domain> and IP:127.0.0.1, and NO SAN for the node's LAN
# address, so a numeric LAN rpc_addr here binds cleanly and then fails
# verification for every caller that is not on loopback. The name is the
# value a colony can actually run; a LAN address is not.
#
# The previous default was the wildcard 0.0.0.0, which is a perfectly
# legal bind address and was shipped here as documented guidance. That is
# what produced the incident this default now records: a wildcard binds
# perfectly and then refuses every connection aimed at it, so as soon as
# the same value was carried into the fields that DIAL managerd
# (restshimd's and frontend's manager_addr, and raftd's confirm hook,
# which reads rpc_addr out of this very file) those daemons logged, live
# on a real node:
#
#	cannot reach managerd at 0.0.0.0:17700 (connecting to
#	0.0.0.0:17700: dial tcp 0.0.0.0:17700: connect: connection
#	refused)
#
# while managerd itself stayed healthy, because its listener really was
# fine. Nothing crashes, nothing restarts, nothing points at the config
# file - the only symptom is an unreachable address in a working config.
# Choose 0.0.0.0 here only when something genuinely must reach managerd
# from off-host, and with the consequences above in mind.
NODE_RPC_ADDR?=		127.0.0.1:17700

# NODE_HTTP_ADDR is the frontend's web UI, and the wildcard here is
# DELIBERATE and must stay that way. The browser runs on the operator's
# own machine, not on the Comb, so a loopback-only frontend serves nobody;
# this is the operator-facing surface and is meant to be reachable from
# the LAN. It is the one address in this block that is not loopback for a
# real reason - do not "fix" it alongside the two around it.
NODE_HTTP_ADDR?=	0.0.0.0:8080

# NODE_REST_ADDR defaults to loopback, which is also internal/
# restshimdconfig's own code default (Config.Defaults), so setup-quick and
# a hand-written config agree. It is not exposed because restshimd is a
# full read/write control API - create/update/delete VMs, jails and
# networks, migrate, upload ISOs - and it has NO AUTHENTICATION OF ITS
# OWN: it holds no key and checks none, it forwards each caller's
# Authorization header straight through to managerd as gRPC metadata (see
# authContext in internal/restshim/server.go). Anything that can open a
# socket to 8081 can therefore drive the whole colony, as whoever it can
# authenticate as. Nothing inside the colony calls it either; it exists
# for external tooling (curl, Terraform, CI) that an operator points at
# it on purpose. To use it from another machine, forward the port over
# SSH rather than widening the bind:
#
#	ssh -L 8081:127.0.0.1:8081 <comb>
NODE_REST_ADDR?=	127.0.0.1:8081

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
# update installs every binary, then restarts only the two daemons that
# cannot cost the colony its quorum. It is the target that is safe to run
# on every Comb in a row without thinking, which is exactly why managerd
# and raftd are not in it. See the split above and ADR-0141.
.PHONY: update
update: install
	@set -e; \
	for S in ${UPDATE_RESTART_SRCS}; do \
		service apiary_$$S restart; \
	done
	@echo "" ; \
	echo "update: frontend and restshimd restarted on `hostname`." ; \
	echo "  managerd and raftd were installed but deliberately NOT" ; \
	echo "  restarted. Run 'make force-restart' here to restart them -" ; \
	echo "  one Comb at a time, and never on the leader as part of a sweep."

# force-restart restarts the two daemons `update` leaves alone, and it is
# named for what it gives up to do that: it hands `service` a restart
# directly, so the ADR-0125 guardrail is bypassed completely. No lease is
# reserved, no quorum-safety evaluation runs, and nothing coordinates with
# the other Combs - this Makefile has no client that speaks the guarded
# RPCs, and adding one is the other answer to the same problem (ADR-0125).
#
# That is only safe because it is a separate, named, per-Comb act. Running
# it across the colony in one sweep is the failure this target is shaped to
# make inconvenient, not impossible: nothing in a Makefile can know what
# the other Combs are doing. So the recipe says so, out loud, every time.
#
# The post-restart assertion is not ceremony. managerd is restarted first
# precisely so that raftd's confirm-on-startup hook has something to talk
# to, and if managerd fails to come back the loop stops there rather than
# restarting raftd into a node whose managerd is down - which is how a
# restart lease would fail to confirm and, having no TTL, block the
# cluster until an operator forced it clear.
#
# WHY sockstat(1) AND NOT `service <name> status`
#
# The assertion used to be `service apiary_<svc> status`, and on these
# Combs that call is not a measurement. Measured on brood and drone,
# `service apiary_managerd status`, `service apiary_raftd status`,
# `service apiary_frontend status` and `service apiary_restshimd status`
# all report not running, on every check, while all four daemons are
# demonstrably up and listening. So the old check was guaranteed to
# reach its own failure branch on the first service, every time: it
# restarted managerd, burned the full 15s, printed the failure and
# exited 1 having never touched raftd - producing by itself the exact
# half-restarted managerd/raftd pair this target exists to prevent, and
# leaving a pending-restart record for raftd that no restart ever
# confirmed. (The rc.d scripts do declare a pidfile and pass it to
# daemon(8) as -P; why status misreports is not this Makefile's problem,
# and adding a pidfile test would only add a second way to be wrong.)
#
# sockstat(1) is the check measured to be truthful here: `sockstat -4 -l`
# lists the daemon's own listening socket. Each service's port is fixed
# by ADR-0108 and named in internal/frontend/fixedport.go (raftd 17600,
# managerd 17700, frontend 8080, restshimd 8081), and the defaults in
# internal/raft/config.go (DefaultBindAddr 127.0.0.1:17600) and
# cmd/managerd/main.go (127.0.0.1:17700) agree with them. A wrong host
# bind does not change the port, so the check is the port and only the
# port. If it still never appears, that is a real failure and is
# reported as one - including which services were already restarted,
# because which build this Comb is now running depends on it.
#
# The port map is resolved for every service in FORCE_RESTART_SRCS
# before the first restart rather than inside the loop, so a service
# with no known port is refused having touched nothing. A third daemon
# added to that list later would otherwise be discovered halfway
# through, with the first daemon already restarted and no record
# explaining why.
#
# The match is `:<port>` followed by whitespace, which does depend on
# sockstat's column layout. If that layout ever changes, the failure
# mode is a timeout and a loud refusal rather than a false pass, which
# is the safe direction for the only confirmation this target has: a
# check that cannot see the listener cannot confirm the restart, and
# says so.
.PHONY: force-restart
force-restart:
	@echo "force-restart: about to restart ${FORCE_RESTART_SRCS} on `hostname`" >&2 ; \
	echo "  by handing 'service' a restart directly. This acquires NO" >&2 ; \
	echo "  restart lease, runs NO quorum preflight, and coordinates" >&2 ; \
	echo "  with NO other Comb. Restarting raftd on more than one Comb at" >&2 ; \
	echo "  once, or on the current leader, can cost the cluster its" >&2 ; \
	echo "  quorum. For a coordinated restart use the Machine page's" >&2 ; \
	echo "  per-service control, which reserves a real cluster-wide lease." >&2 ; \
	echo "" >&2 ; \
	echo "  It does still leave a record: each service gets the same" >&2 ; \
	echo "  pending-restart note a leased restart would, with lease_id 0," >&2 ; \
	echo "  so the guardrail's 600s cooldown learns a restart happened" >&2 ; \
	echo "  here and blocks a second one on another Comb. It never takes" >&2 ; \
	echo "  a lease and never releases one." >&2 ; \
	echo "" >&2
	@set -e; \
	plan= ; \
	for S in ${FORCE_RESTART_SRCS}; do \
		case $$S in \
		managerd) plan="$${plan:+$${plan} }managerd:17700" ;; \
		raftd)    plan="$${plan:+$${plan} }raftd:17600" ;; \
		*) \
			echo "force-restart: no known listener port for apiary_$$S," >&2 ; \
			echo "  and this target will not guess one. NOTHING has been" >&2 ; \
			echo "  restarted: add the daemon's fixed port to the case" >&2 ; \
			echo "  above (ADR-0108, internal/frontend/fixedport.go)." >&2 ; \
			exit 1 ; \
		esac ; \
	done ; \
	echo "  restart plan, each confirmed by its own listener port:" >&2 ; \
	echo "    $$plan  (service:port, in this order)" >&2 ; \
	restarted= ; \
	for pair in $$plan ; do \
		S=$${pair%%:*} ; port=$${pair#*:} ; \
		echo "restarting apiary_$$S (waiting for port $$port) ..." ; \
		sh scripts/record-forced-restart.sh "$$S" ; \
		service apiary_$$S restart ; \
		restarted="$${restarted:+$${restarted} }$$S" ; \
		i=0 ; \
		while [ $$i -lt 15 ] ; do \
			sockstat -4 -l 2>/dev/null | grep -q ":$$port[[:space:]]" && break ; \
			i=$$((i+1)) ; sleep 1 ; \
		done ; \
		if [ $$i -ge 15 ] ; then \
			echo "apiary_$$S did not open its listener port $$port within 15s." >&2 ; \
			echo "  That is a sockstat(1) port check, not a 'service" >&2 ; \
			echo "  status' check - 'status' is not trustworthy on this" >&2 ; \
			echo "  host, which is why the port is checked instead." >&2 ; \
			echo "  Stopping here rather than restarting the next" >&2 ; \
			echo "  service: a half-restarted managerd/raftd pair is" >&2 ; \
			echo "  exactly the state that strands a restart lease." >&2 ; \
			echo "  ALREADY RESTARTED on this Comb, this run:" >&2 ; \
			echo "    $$restarted" >&2 ; \
			echo "  NOT restarted: every service after apiary_$$S in" >&2 ; \
			echo "  the plan above, so this Comb is now running a mix" >&2 ; \
			echo "  of the old and the new build. Finish or roll back" >&2 ; \
			echo "  before touching the next Comb." >&2 ; \
			echo "  See /var/log/apiary/$$S.log" >&2 ; \
			exit 1 ; \
		fi ; \
		echo "  apiary_$$S is listening on port $$port" ; \
	done
	@echo "" ; \
	echo "force-restart: done on `hostname`." ; \
	echo "  Confirm the running build, not the one on disk:" ; \
	echo "    grep build= /var/log/apiary/managerd.log /var/log/apiary/raftd.log" ; \
	echo "      | tail -2" ; \
	echo "  and check the Machine page's colony view before moving on to" ; \
	echo "  the next Comb."
