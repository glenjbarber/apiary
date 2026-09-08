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
# themselves: the rc.d scripts (etc/rc.d/apiary_*) and a PAM policy file
# for the frontend's real login - see docs/bootstrap.md's Step 11 for
# the full walkthrough this mirrors, including why the PAM file is
# written with printf rather than a pasted heredoc (tab corruption).
# Safe to re-run: the rc.d scripts/sysrc enables are always refreshed to
# match this checkout, but the PAM file is only written if absent, so a
# later hand-edited /etc/pam.d/apiary is never clobbered by a re-run.
setup: setup-rcd setup-pam

setup-rcd:
	sudo cp etc/rc.d/apiary_* /usr/local/etc/rc.d/
	sudo chmod 555 /usr/local/etc/rc.d/apiary_*
	sudo sysrc apiary_raftd_enable=YES apiary_managerd_enable=YES apiary_frontend_enable=YES apiary_restshimd_enable=YES

setup-pam:
	test -f /etc/pam.d/${PAM_SERVICE} || \
		sudo sh -c "printf 'auth required pam_unix.so no_warn\\naccount required pam_unix.so\\n' > /etc/pam.d/${PAM_SERVICE}"
	@echo "PAM policy at /etc/pam.d/${PAM_SERVICE} - pass -pam-service ${PAM_SERVICE} and a -role-map to frontend to enable real login (see docs/bootstrap.md Step 11)."
