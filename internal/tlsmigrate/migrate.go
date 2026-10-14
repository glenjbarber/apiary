// Package tlsmigrate is the plan/apply logic behind `apiaryctl
// migrate-tls-dir` (issue #99): moving an existing Comb's serving
// certificate and key from the old, sibling TLS directory
// (/usr/local/etc/apiary-tls) into the new default inside the config
// directory (/usr/local/etc/apiary/tls), and rewriting the three config
// files that name the old paths to name the new ones instead.
//
// This package exists because the default changed
// (internal/hostinstall.DefaultPaths), but an existing Comb's config
// files already have tls_cert/tls_key/manager_tls_ca pointing at the
// old location, and nothing may silently rewrite those for anyone -
// see internal/hostinstall's own preservation rule. Moving the files
// out from under a live Comb is instead a deliberate, one-command,
// explicitly-invoked action, not something `apiaryctl install` ever
// does on a re-run.
//
// The two rules this package is built around, because getting either
// wrong on a live Comb is the kind of incident issue #99 exists to
// prevent:
//
//   - never print or log the private key's contents. Plan and Result
//     carry paths only, exactly like internal/hostinstall's own
//     "(not shown)" convention for key.pem.
//   - permissions carry over exactly: the private key stays 0600, the
//     new directory stays root-only (0700), whatever the old one's mode
//     was. This package reads the old file's mode and re-applies it
//     rather than assuming what it ought to be.
package tlsmigrate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
)

// Paths is the one directory this package needs to find everything
// else: the three config files it rewrites, and (from their own
// tls_cert/tls_key/manager_tls_ca fields) the certificate and key it
// moves. A struct rather than a constant so a test can point it at a
// fixture directory instead of the real /usr/local/etc/apiary.
type Paths struct {
	ConfigDir string // /usr/local/etc/apiary
}

// DefaultPaths matches internal/hostinstall.DefaultPaths' ConfigDir.
func DefaultPaths() Paths {
	return Paths{ConfigDir: "/usr/local/etc/apiary"}
}

func (p Paths) withDefaults() Paths {
	if p.ConfigDir == "" {
		p.ConfigDir = DefaultPaths().ConfigDir
	}
	return p
}

func (p Paths) managerdConfig() string  { return filepath.Join(p.ConfigDir, "managerd.json") }
func (p Paths) frontendConfig() string  { return filepath.Join(p.ConfigDir, "frontend.json") }
func (p Paths) restshimdConfig() string { return filepath.Join(p.ConfigDir, "restshimd.json") }

// newTLSDir is internal/hostinstall.DefaultPaths' own new default,
// restated here rather than imported: importing internal/hostinstall
// from this package for one string would pull in its whole plan/apply
// machinery for a constant that is this package's own contract with
// it, not a derived fact.
func (p Paths) newTLSDir() string { return filepath.Join(p.ConfigDir, "tls") }

// fileRef is one config file's own copy of one TLS path field -
// which file, which JSON field, and the path it currently names. It
// is how Plan reports what it found without needing three near-
// identical structs for managerd/frontend/restshimd.
type fileRef struct {
	file  string // config file path, for the report
	field string // JSON field name, for the report
	value string // the path currently on disk
}

// Plan is what a migration would do, decided by reading the three
// config files and nothing else - no file is moved and no config file
// is rewritten until Apply runs.
type Plan struct {
	paths Paths

	// NoOp is true when nothing needs to move: either no config file
	// names a TLS path outside the new default directory, or every one
	// that does already names the new directory.
	NoOp      bool
	NoOpWhy   string
	OldTLSDir string // "" when NoOp
	NewTLSDir string

	// refs are every TLS-path field this plan found pointing at
	// OldTLSDir, across all three files - what Apply rewrites.
	refs []fileRef

	// moves are the distinct basenames under OldTLSDir that Apply moves
	// into NewTLSDir - normally cert.pem and key.pem, but this does not
	// assume those exact names: whatever tls_cert/tls_key/manager_tls_ca
	// actually name is what moves.
	moves []string
}

// Files reports the config files this plan would rewrite, sorted and
// de-duplicated, for a report line.
func (p *Plan) Files() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range p.refs {
		if !seen[r.file] {
			seen[r.file] = true
			out = append(out, r.file)
		}
	}
	return out
}

// Moves reports the basenames this plan would move from OldTLSDir to
// NewTLSDir - never a full path with directory, and never file
// contents, so printing this list is always safe.
func (p *Plan) Moves() []string { return append([]string(nil), p.moves...) }

// New reads the three config files and decides what a migration would
// do. It writes nothing.
func New(paths Paths) (*Plan, error) {
	paths = paths.withDefaults()
	p := &Plan{paths: paths, NewTLSDir: paths.newTLSDir()}

	managerd, err := (&nodeconfig.Manager{Path: paths.managerdConfig()}).Load()
	if err != nil {
		return nil, fmt.Errorf("tlsmigrate: reading %s: %w", paths.managerdConfig(), err)
	}
	frontend, err := (&frontendconfig.Manager{Path: paths.frontendConfig()}).Load()
	if err != nil {
		return nil, fmt.Errorf("tlsmigrate: reading %s: %w", paths.frontendConfig(), err)
	}
	restshimd, err := (&restshimdconfig.Manager{Path: paths.restshimdConfig()}).Load()
	if err != nil {
		return nil, fmt.Errorf("tlsmigrate: reading %s: %w", paths.restshimdConfig(), err)
	}

	candidates := []fileRef{
		{paths.managerdConfig(), "tls_cert", managerd.TLSCert},
		{paths.managerdConfig(), "tls_key", managerd.TLSKey},
		{paths.frontendConfig(), "manager_tls_ca", frontend.ManagerTLSCA},
		{paths.frontendConfig(), "tls_cert", frontend.TLSCert},
		{paths.frontendConfig(), "tls_key", frontend.TLSKey},
		{paths.restshimdConfig(), "manager_tls_ca", restshimd.ManagerTLSCA},
		{paths.restshimdConfig(), "tls_cert", restshimd.TLSCert},
		{paths.restshimdConfig(), "tls_key", restshimd.TLSKey},
	}

	var oldDir string
	var toMove []fileRef
	movedNames := map[string]bool{}
	for _, c := range candidates {
		if c.value == "" {
			continue
		}
		dir, base := filepath.Split(c.value)
		dir = filepath.Clean(dir)
		if dir == p.NewTLSDir {
			// Already migrated for this field.
			continue
		}
		if oldDir == "" {
			oldDir = dir
		} else if dir != oldDir {
			// Two fields disagree about where the old material lives -
			// not this tool's call to make. Refusing outright is the
			// same posture internal/hostinstall takes for a node_id
			// disagreement: a dispute about a fact this install relies
			// on blocks the whole migration rather than guessing which
			// field is right.
			return nil, fmt.Errorf(
				"tlsmigrate: %s's %s names a directory (%s) that disagrees with %s's own TLS directory (%s) - fix the config files by hand so they agree, then re-run",
				c.file, c.field, dir, toMove[0].file, oldDir)
		}
		toMove = append(toMove, c)
		movedNames[base] = true
	}

	if oldDir == "" {
		p.NoOp = true
		p.NoOpWhy = fmt.Sprintf("every TLS path field already names %s (or none is set) - nothing to move", p.NewTLSDir)
		return p, nil
	}

	p.OldTLSDir = oldDir
	p.refs = toMove
	for name := range movedNames {
		p.moves = append(p.moves, name)
	}
	return p, nil
}

// Apply carries out the plan: moves the files, rewrites the config
// fields, and returns a Result with no restart yet performed -
// restarting the daemons is the caller's job (see Restart), done
// after Apply succeeds and never before, so a failed move never leaves
// a Comb restarted onto a certificate that is not where its config
// says it is.
func (p *Plan) Apply() error {
	if p.NoOp {
		return nil
	}
	if err := os.MkdirAll(p.paths.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("tlsmigrate: creating %s: %w", p.paths.ConfigDir, err)
	}
	if err := os.MkdirAll(p.NewTLSDir, 0o700); err != nil {
		return fmt.Errorf("tlsmigrate: creating %s: %w", p.NewTLSDir, err)
	}
	// Directory stays root-only regardless of what MkdirAll's umask-
	// adjusted mode landed on, in case it already existed with
	// something wider.
	if err := os.Chmod(p.NewTLSDir, 0o700); err != nil {
		return fmt.Errorf("tlsmigrate: setting %s to root-only: %w", p.NewTLSDir, err)
	}

	for _, name := range p.moves {
		if err := moveFile(filepath.Join(p.OldTLSDir, name), filepath.Join(p.NewTLSDir, name)); err != nil {
			return err
		}
	}

	if err := rewriteConfigs(p.paths, p.refs, p.NewTLSDir); err != nil {
		return err
	}
	return nil
}

// moveFile moves one file, preserving its exact mode. os.Rename is
// used when old and new are on the same filesystem - the common case,
// since both live under /usr/local/etc on every install this project
// documents (docs/bootstrap.md) - which is also atomic: there is no
// window where the file exists at neither path or at both with
// different content. When the two directories are NOT on the same
// filesystem, os.Rename fails with a cross-device error and this falls
// back to copy-then-chmod-then-remove-original, in that order, so a
// failure at any point leaves the original still in place rather than
// deleted with no copy to show for it.
//
// Nothing here reads the file for any purpose but copying its bytes,
// and nothing here is ever printed - the caller never sees file
// content, only the paths that were asked for.
func moveFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("tlsmigrate: %s: %w", src, err)
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return fmt.Errorf("tlsmigrate: moving %s to %s: %w", src, dst, err)
	}
	if err := copyFilePreservingMode(src, dst, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("tlsmigrate: removing original %s after copying it to %s: %w", src, dst, err)
	}
	return nil
}

func copyFilePreservingMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("tlsmigrate: opening %s: %w", src, err)
	}
	defer in.Close()
	// O_EXCL: never silently overwrite something already at dst - that
	// would be a second, unrelated file's bytes lost with no error.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("tlsmigrate: creating %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("tlsmigrate: copying %s to %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("tlsmigrate: syncing %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("tlsmigrate: closing %s: %w", dst, err)
	}
	// OpenFile's mode is subject to umask; chmod explicitly so the copy
	// carries EXACTLY the source's mode (0600 on a private key), not
	// whatever umask left it at.
	if err := os.Chmod(dst, mode); err != nil {
		return fmt.Errorf("tlsmigrate: setting permissions on %s: %w", dst, err)
	}
	return nil
}

// rewriteConfigs points every field Plan found at the new directory,
// through each package's own Load/Save - never a raw JSON edit - so
// the same validation and atomic-write guarantees every other writer
// of these files already gets apply here too.
func rewriteConfigs(paths Paths, refs []fileRef, newDir string) error {
	byFile := map[string][]string{}
	var order []string
	for _, r := range refs {
		if _, ok := byFile[r.file]; !ok {
			order = append(order, r.file)
		}
		byFile[r.file] = append(byFile[r.file], r.field)
	}
	for _, file := range order {
		fields := byFile[file]
		switch file {
		case paths.managerdConfig():
			m := &nodeconfig.Manager{Path: file}
			cfg, err := m.Load()
			if err != nil {
				return fmt.Errorf("tlsmigrate: re-reading %s: %w", file, err)
			}
			for _, f := range fields {
				switch f {
				case "tls_cert":
					cfg.TLSCert = filepath.Join(newDir, filepath.Base(cfg.TLSCert))
				case "tls_key":
					cfg.TLSKey = filepath.Join(newDir, filepath.Base(cfg.TLSKey))
				}
			}
			if err := m.Save(cfg); err != nil {
				return fmt.Errorf("tlsmigrate: writing %s: %w", file, err)
			}
		case paths.frontendConfig():
			m := &frontendconfig.Manager{Path: file}
			cfg, err := m.Load()
			if err != nil {
				return fmt.Errorf("tlsmigrate: re-reading %s: %w", file, err)
			}
			for _, f := range fields {
				switch f {
				case "manager_tls_ca":
					cfg.ManagerTLSCA = filepath.Join(newDir, filepath.Base(cfg.ManagerTLSCA))
				case "tls_cert":
					cfg.TLSCert = filepath.Join(newDir, filepath.Base(cfg.TLSCert))
				case "tls_key":
					cfg.TLSKey = filepath.Join(newDir, filepath.Base(cfg.TLSKey))
				}
			}
			if err := m.Save(cfg); err != nil {
				return fmt.Errorf("tlsmigrate: writing %s: %w", file, err)
			}
		case paths.restshimdConfig():
			m := &restshimdconfig.Manager{Path: file}
			cfg, err := m.Load()
			if err != nil {
				return fmt.Errorf("tlsmigrate: re-reading %s: %w", file, err)
			}
			for _, f := range fields {
				switch f {
				case "manager_tls_ca":
					cfg.ManagerTLSCA = filepath.Join(newDir, filepath.Base(cfg.ManagerTLSCA))
				case "tls_cert":
					cfg.TLSCert = filepath.Join(newDir, filepath.Base(cfg.TLSCert))
				case "tls_key":
					cfg.TLSKey = filepath.Join(newDir, filepath.Base(cfg.TLSKey))
				}
			}
			if err := m.Save(cfg); err != nil {
				return fmt.Errorf("tlsmigrate: writing %s: %w", file, err)
			}
		}
	}
	return nil
}
