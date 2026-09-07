# Makefile
SRCS=		apiaryinstall \
			raftd \
			managerd \
			frontend \
			restshimd

build:
	for S in ${SRCS} ; \
		do go build -buildvcs=false -o $$S ./cmd/$$S ;\
	done

clean:
	for S in ${SRCS} ; \
		do rm -f $$S ; \
	done
