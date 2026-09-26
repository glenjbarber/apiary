// Command restshimd serves internal/restshim's REST/JSON translation of
// managerd's external gRPC API. Like cmd/frontend, it's just another
// client of ManagerService, dialed the same way - it never talks to
// raftd directly. Unlike cmd/frontend, it attaches no API key of its
// own: each caller's own "Authorization" header is forwarded straight
// through to managerd as gRPC metadata (see internal/restshim's
// authContext), since restshim has no single application-level
// identity the way the UI does - it's meant to sit in front of
// external tooling (curl, Terraform, CI) where each caller presents
// its own key.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/glenjbarber/apiary/internal/buildinfo"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/managerlink"
	"github.com/glenjbarber/apiary/internal/restshim"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
	"github.com/glenjbarber/apiary/internal/tlsdial"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("restshimd: %v", err)
	}
}

const (
	// managerCheckGrace bounds how long startup waits for managerd to
	// answer the transport-scheme check before serving anyway. Long
	// enough to cover managerd starting a moment later (the two are
	// co-located and either can win the race), short enough that a
	// restshimd restart never hangs on a managerd that is not coming
	// back. A managerd that does answer - the normal case - is one
	// loopback connect and is done in well under a millisecond.
	managerCheckGrace = 5 * time.Second

	// managerLinkWatchInterval is how often the link is re-verified after
	// startup, so a managerd that is restarted with TLS turned on (or off)
	// is noticed and named in the log rather than surfacing only as a
	// stream of anonymous 502s. See managerlink.Checker.Watch.
	managerLinkWatchInterval = 30 * time.Second
)

// verifier is the startup check cmd/restshimd depends on, as an interface
// so the decision it makes - refuse to start, or start degraded and say so
// - can be tested without a live managerd and without a real socket.
type verifier interface {
	Verify(ctx context.Context, grace time.Duration) error
}

func run() error {
	// These two never had flags of their own, so they never called
	// flag.Parse. Registering -version is what introduces the first
	// one; the parse has to happen for it to take effect at all.
	buildinfo.RegisterVersionFlag(flag.CommandLine)
	flag.Parse()

	if buildinfo.VersionRequested() {
		fmt.Print(buildinfo.Report("restshimd"))
		return nil
	}

	cfg, err := (&restshimdconfig.Manager{}).Load()
	if err != nil {
		return fmt.Errorf("reading %s: %w", restshimdconfig.DefaultPath, err)
	}

	link := managerlink.New(managerlink.Config{
		Addr:        cfg.ManagerAddr,
		UseTLS:      cfg.ManagerTLS,
		CAFile:      cfg.ManagerTLSCA,
		ServerName:  cfg.ManagerTLSServerName,
		ProcessName: "restshimd",
		ConfigPath:  restshimdconfig.DefaultPath,
	})

	// grpc.NewClient is lazy - it resolves the target and returns a
	// ClientConn without ever opening a socket - so a transport-scheme
	// disagreement with managerd cannot possibly show up below. Left
	// unchecked it surfaces as "error reading server preface: EOF" on
	// every single call, reported to REST callers as a bare 502, with
	// nothing in either daemon's log saying the two config files
	// disagreed. Ask managerd what it actually speaks before serving.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := checkManagerLink(ctx, link, managerCheckGrace, log.Printf); err != nil {
		return err
	}
	go link.Watch(ctx, managerLinkWatchInterval, log.Printf)

	dialCreds, err := tlsdial.ManagerDialOption(cfg.ManagerTLS, cfg.ManagerTLSCA, cfg.ManagerTLSServerName)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(cfg.ManagerAddr, dialCreds)
	if err != nil {
		return fmt.Errorf("dialing managerd at %s: %w", cfg.ManagerAddr, err)
	}
	defer conn.Close()

	// WithDiagnosis is what makes a later failure honest: the scheme
	// check ran once at startup, but managerd can be restarted with a
	// different scheme at any time, and until now that surfaced as the
	// same anonymous 502 as a managerd outage. The diagnosis re-observes
	// the endpoint (bounded, cached) rather than trusting the startup
	// verdict, and names the cause and the fix when there is one.
	srv := restshim.NewServer(rpcpb.NewManagerServiceClient(conn),
		restshim.WithDiagnosis(func(err error) restshim.LinkDiagnosis {
			d := link.Explain(err)
			return restshim.LinkDiagnosis{Class: d.Class, Detail: d.Detail, Permanent: d.Permanent}
		}))

	log.Printf("restshimd: %s listening on %s (manager-addr=%s, manager-tls=%v, tls=%v)", buildinfo.String(), cfg.HTTPAddr, cfg.ManagerAddr, cfg.ManagerTLS, cfg.TLSCert != "")
	if cfg.TLSCert != "" || cfg.TLSKey != "" {
		if err := requireTLSPair(cfg.TLSCert, cfg.TLSKey); err != nil {
			return err
		}
		return http.ListenAndServeTLS(cfg.HTTPAddr, cfg.TLSCert, cfg.TLSKey, srv)
	}
	return http.ListenAndServe(cfg.HTTPAddr, srv)
}

// checkManagerLink turns the startup check's answer into a decision.
//
// A permanent failure - the two files disagree about the transport
// scheme, or the certificate does not verify - refuses to start. Serving
// anyway would mean serving 100% failures, and worse, a caller retrying a
// 500 that no retry can fix. Everything else (managerd not up yet, a PF
// rule in the way) is not evidence of a misconfiguration, so it starts
// degraded and says so loudly: the TLS setting is UNVERIFIED, every call
// will fail until managerd answers, and neither of those is a statement
// about whether the config is right.
func checkManagerLink(ctx context.Context, v verifier, grace time.Duration, logf func(string, ...any)) error {
	err := v.Verify(ctx, grace)
	if err == nil {
		return nil
	}
	if managerlink.IsPermanent(err) {
		return err
	}
	logf("restshimd: WARNING: %v", err)
	logf("restshimd: WARNING: starting in a DEGRADED state: every call to managerd will fail until it is " +
		"reachable, and the manager_tls setting is UNVERIFIED until then - this is not a statement that the " +
		"config is wrong, only that nothing has been able to check it")
	return nil
}

// requireTLSPair rejects a config setting only one of tls_cert/tls_key -
// serving TLS needs both, and silently falling back to plaintext
// because only one was set would be a confusing way to fail.
func requireTLSPair(cert, key string) error {
	if cert == "" || key == "" {
		return fmt.Errorf("both tls_cert and tls_key must be set together")
	}
	return nil
}
