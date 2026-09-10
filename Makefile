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
# and a PAM policy file for the frontend's real login - see
# docs/bootstrap.md's Step 11 for the full walkthrough this mirrors,
# including why the PAM file is written with printf rather than a
# pasted heredoc (tab corruption).
# Safe to re-run: the directories/rc.d scripts/sysrc enables are always
# refreshed to match this checkout, but the PAM file is only written if
# absent, so a later hand-edited /etc/pam.d/apiary is never clobbered
# by a re-run.
setup: setup-dirs setup-rcd setup-pam

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
