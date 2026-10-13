// Package tlscache serves a certificate/key pair that can be replaced
// on disk - by a renewal, by hand, by any issuer - without a process
// restart.
//
// managerd (credentials.NewServerTLSFromFile) and frontend/restshimd
// (http.Server.ListenAndServeTLS) each load their serving certificate
// once, at startup. Nothing notices a replaced file afterwards, so
// every renewal needs `service apiary_X restart` to take effect - for
// frontend that also drops the operator's session and the web UI for a
// few seconds. Cache fixes that: wire its GetCertificate method into
// tls.Config instead of a static Certificates slice, and a renewed
// pair is picked up on the next TLS handshake, with no restart.
package tlscache

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// Cache loads a certificate/key pair from two fixed paths and re-reads
// them only when either file's size or modification time has changed
// since the last successful load - a stat on every call, a read only
// when something changed, which is the common case of nothing having
// changed between two TLS handshakes.
//
// A reload that fails - the file is gone, the PEM is corrupt, or the
// certificate and key do not pair - leaves the previously loaded
// certificate serving and logs why, rather than failing the handshake.
// This also makes Cache safe against origincert.WritePair's own
// install order: it renames the certificate into place, then the key,
// so a reload landing in that narrow window sees a mismatched pair,
// fails to parse it, and keeps serving the old (still consistent)
// pair - the very next handshake reloads correctly once both renames
// have happened.
type Cache struct {
	certPath, keyPath string
	logf              func(format string, args ...any)

	mu       sync.Mutex
	cert     *tls.Certificate
	certStat fileStat
	keyStat  fileStat
}

type fileStat struct {
	size    int64
	modTime time.Time
}

func (a fileStat) equal(b fileStat) bool {
	return a.size == b.size && a.modTime.Equal(b.modTime)
}

func statOf(path string) (fileStat, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStat{}, err
	}
	return fileStat{size: info.Size(), modTime: info.ModTime()}, nil
}

// New loads certPath/keyPath once, synchronously, and fails if that
// initial pair cannot be loaded - the same fail-at-startup behavior
// credentials.NewServerTLSFromFile and ListenAndServeTLS already have,
// so a bad pair still refuses to start rather than silently serving
// nothing. logf receives one line per failed reload attempt after
// that; nil defaults to log.Printf.
func New(certPath, keyPath string, logf func(format string, args ...any)) (*Cache, error) {
	if logf == nil {
		logf = log.Printf
	}
	c := &Cache{certPath: certPath, keyPath: keyPath, logf: logf}
	cert, certStat, keyStat, err := c.load()
	if err != nil {
		return nil, err
	}
	c.cert, c.certStat, c.keyStat = cert, certStat, keyStat
	return c, nil
}

func (c *Cache) load() (*tls.Certificate, fileStat, fileStat, error) {
	certStat, err := statOf(c.certPath)
	if err != nil {
		return nil, fileStat{}, fileStat{}, fmt.Errorf("stat %s: %w", c.certPath, err)
	}
	keyStat, err := statOf(c.keyPath)
	if err != nil {
		return nil, fileStat{}, fileStat{}, fmt.Errorf("stat %s: %w", c.keyPath, err)
	}
	cert, err := tls.LoadX509KeyPair(c.certPath, c.keyPath)
	if err != nil {
		return nil, fileStat{}, fileStat{}, err
	}
	return &cert, certStat, keyStat, nil
}

// GetCertificate implements the signature tls.Config.GetCertificate
// expects. Wire it in directly: &tls.Config{GetCertificate:
// cache.GetCertificate} - never set Certificates on the same config,
// which would take precedence over GetCertificate and defeat the
// reload entirely.
func (c *Cache) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	certStat, err := statOf(c.certPath)
	if err != nil {
		c.logf("tlscache: stat %s: %v; serving the previously loaded certificate", c.certPath, err)
		return c.cert, nil
	}
	keyStat, err := statOf(c.keyPath)
	if err != nil {
		c.logf("tlscache: stat %s: %v; serving the previously loaded certificate", c.keyPath, err)
		return c.cert, nil
	}
	if certStat.equal(c.certStat) && keyStat.equal(c.keyStat) {
		return c.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(c.certPath, c.keyPath)
	if err != nil {
		c.logf("tlscache: reloading %s / %s: %v; serving the previously loaded certificate", c.certPath, c.keyPath, err)
		return c.cert, nil
	}
	c.cert, c.certStat, c.keyStat = &cert, certStat, keyStat
	c.logf("tlscache: reloaded certificate from %s", c.certPath)
	return c.cert, nil
}
