package tlsmigrate

import (
	"fmt"
	"os/exec"
)

// Restarter restarts one rc.d service by name. Production code uses
// ServiceRestarter; tests substitute a fake so nothing here ever shells
// out to service(8) for real.
type Restarter interface {
	Restart(service string) error
}

// ServiceRestarter runs `service <name> restart`, the same mechanism
// docs/bootstrap.md's own start line and internal/forcerestart both
// use.
type ServiceRestarter struct{}

func (ServiceRestarter) Restart(service string) error {
	cmd := exec.Command("service", service, "restart")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tlsmigrate: service %s restart: %w: %s", service, err, out)
	}
	return nil
}

// RestartOrder is the safe order to restart the daemons that serve the
// certificate/key this package just moved: managerd first, because
// frontend and restshimd both dial IT over TLS (manager_tls_ca pins
// managerd's own certificate) and a frontend/restshimd restarted first
// would briefly dial a managerd still serving the OLD file descriptor
// for a certificate whose path just changed under it. This mirrors
// docs/bootstrap.md's own daemon start order (raftd, managerd,
// frontend, restshimd) for the two daemons this migration touches.
var RestartOrder = []string{"apiary_managerd", "apiary_frontend", "apiary_restshimd"}

// Restart restarts every daemon in RestartOrder whose own config file
// this plan rewrote, in order, stopping at the first failure so a
// Comb is never left worse off than reporting the one that failed and
// no more.
func (p *Plan) Restart(r Restarter) ([]string, error) {
	touched := map[string]bool{}
	for _, f := range p.Files() {
		switch f {
		case p.paths.managerdConfig():
			touched["apiary_managerd"] = true
		case p.paths.frontendConfig():
			touched["apiary_frontend"] = true
		case p.paths.restshimdConfig():
			touched["apiary_restshimd"] = true
		}
	}
	var restarted []string
	for _, svc := range RestartOrder {
		if !touched[svc] {
			continue
		}
		if err := r.Restart(svc); err != nil {
			return restarted, err
		}
		restarted = append(restarted, svc)
	}
	return restarted, nil
}

// AcmeHookNote is printed after every successful migration that moved
// files, unconditionally. apiary's own tree documents no fixed path or
// format for an acme.sh renewal hook (docs/bootstrap.md names Let's
// Encrypt only as "drop the chain/key at ${NODE_TLS_DIR}" by hand, with
// no hook script of its own), so there is nothing this package can
// safely locate and rewrite on an operator's behalf - guessing at a
// cron job or hook file this project does not own is exactly the kind
// of silent action issue #99 asks this tool not to take. This message
// is the documented fallback: a clear instruction instead of a guess.
const AcmeHookNote = `If this Comb renews its certificate with acme.sh (or any other ACME
client) using a renewal hook that installs the new certificate/key
into the old directory, update that hook's install path now, by hand,
to point at the new directory printed above. apiaryctl has no fixed
location for that hook to find and rewrite automatically - it is owned
by whatever renewal tooling you set up, not by Apiary.`
