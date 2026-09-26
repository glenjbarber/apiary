// Package buildinfo reports which build a running Apiary binary
// actually is.
//
// This exists because a deploy and a restart are different events, and
// the filesystem alone cannot tell you which one happened. A binary
// copied over a running executable leaves the new bytes on disk and
// the old ones resident in the process, so an mtime proves nothing
// about what is executing. On a Combs where raftd is deliberately
// excluded from install (see Makefile INSTALL_SRCS_FILTERED) the two
// can stay different indefinitely and still look deployed.
//
// Every daemon prints its build ID on startup, so the running process
// states its own identity, and -version prints the same values for a
// binary on disk without starting anything.
//
// Values are injected at link time by the Makefile:
//
//	go build -ldflags "-X github.com/glenjbarber/apiary/internal/buildinfo.BuildID=<id>"
//
// With no injection (a plain `go build ./...`, or `go run`), every
// value is empty and Uninjected is true. That is reported as
// "unknown" rather than being papered over with a plausible-looking
// placeholder, because a build ID that looks real but is not is worse
// than one that admits it does not know.
package buildinfo

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
)

// Injected at link time by the Makefile's build target. Empty
// otherwise; see the package comment.
var (
	// BuildID is a short, unique-per-build identifier. It is the value
	// to compare between nodes when asking "are these all the same
	// build?".
	BuildID = ""

	// BuildTime is the UTC RFC3339 timestamp of the build.
	BuildTime = ""

	// GitCommit is the commit the binary was built from, when the
	// build had a clean worktree. Empty for a dirty tree or a build
	// made without VCS stamping, which is why -buildvcs=false must be
	// paired with an explicit -X in the Makefile rather than relying
	// on Go's automatic stamping.
	GitCommit = ""

	// Version is a human-facing release or version label. Optional and
	// often empty; BuildID is the field to compare on.
	Version = ""
)

// Uninjected reports whether this binary carries no link-time stamp at
// all. Callers should surface this rather than rendering empty fields
// as though they were values.
func Uninjected() bool {
	return BuildID == "" && BuildTime == "" && GitCommit == "" && Version == ""
}

// String renders the build identity in a stable one-line form suitable
// for a log line or a `-version` flag. The "uninjected" case is stated
// plainly instead of being formatted as an empty value.
func String() string {
	var b strings.Builder
	if Uninjected() {
		b.WriteString("build=unknown (not stamped; built without " +
			"-ldflags -X .../buildinfo.BuildID)")
	} else {
		b.WriteString("build=")
		b.WriteString(orUnknown(BuildID))
		if Version != "" {
			b.WriteString(" version=")
			b.WriteString(Version)
		}
		if GitCommit != "" {
			b.WriteString(" commit=")
			b.WriteString(short(GitCommit))
		}
		if BuildTime != "" {
			b.WriteString(" built=")
			b.WriteString(BuildTime)
		}
	}
	// The platform is appended unconditionally, including in the
	// unstamped case: "I do not know which build this is" is still
	// compatible with knowing what it was compiled for, and that is
	// the fact that matters when a binary may have been
	// cross-compiled from a Mac.
	fmt.Fprintf(&b, " go=%s/%s", runtime.GOOS, runtime.GOARCH)
	return b.String()
}

// VCS returns the VCS revision Go recorded for this build, if any.
// It is a cross-check on GitCommit rather than a replacement: a build
// made with -buildvcs=false has none, and a build made in a dirty
// worktree reports a revision that does not describe the bytes.
func VCS() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}

// ModuleVersion returns this module's own version, or "" if the build
// was not module-aware.
func ModuleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return bi.Main.Version
}

// Toolchain returns the Go toolchain that produced this binary, which
// is worth knowing when a FreeBSD-only failure is suspected and the
// binary may have been cross-compiled from a Mac.
func Toolchain() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return runtime.Version()
	}
	if bi.GoVersion != "" {
		return bi.GoVersion
	}
	return runtime.Version()
}

// Report renders a multi-line, human-facing build report for a
// `-version` flag. Everything here is read from the running binary, so
// running `<binary> -version` describes the bytes on disk and
// `strings <binary>` can be cross-checked against a daemon's startup
// log line to confirm a restart actually happened.
func Report(program string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", program, String())
	fmt.Fprintf(&b, "  program:   %s\n", program)
	fmt.Fprintf(&b, "  go:        %s\n", Toolchain())
	fmt.Fprintf(&b, "  module:    %s\n", orNone(ModuleVersion()))
	fmt.Fprintf(&b, "  vcs:       %s\n", orNone(VCS()))
	if bi, ok := debug.ReadBuildInfo(); ok {
		// ReadBuildInfo also exposes the build settings Go recorded,
		// which is where -trimpath and the GOOS/GOARCH the binary was
		// actually built for are recorded - useful when a binary was
		// cross-compiled and its provenance is in doubt.
		for _, s := range bi.Settings {
			switch s.Key {
			case "-trimpath", "CGO_ENABLED", "GOARCH", "GOOS", "GOAMD64":
				fmt.Fprintf(&b, "  build:     %s=%s\n", s.Key, s.Value)
			}
		}
	}
	return b.String()
}

// showVersion is the destination of the -version flag registered by
// HandleVersionFlag. It is package-level because the flag package
// requires a stable address to bind to.
var showVersion bool

// RegisterVersionFlag adds -version to fs. It MUST be called before
// flag.Parse, or the flag package rejects `-version` as undefined and
// prints usage - which is exactly the failure this split exists to
// prevent.
func RegisterVersionFlag(fs *flag.FlagSet) {
	fs.BoolVar(&showVersion, "version", false,
		"print build identity (build id, commit, build time, Go toolchain) "+
			"and exit without starting any service")
}

// VersionRequested reports whether -version was passed. Call it after
// flag.Parse. Together with RegisterVersionFlag this lets a main put
// the whole check at the very top of run(), before any config read,
// any dial, any data-directory access, or any privilege requirement.
func VersionRequested() bool { return showVersion }

// HandleVersionFlag registers -version on fs and reports whether the
// caller should exit immediately. Convenient only where registration
// genuinely precedes parsing; the daemons that need the check first
// should use RegisterVersionFlag plus VersionRequested instead.
func HandleVersionFlag(fs *flag.FlagSet, program string) (exit bool) {
	RegisterVersionFlag(fs)
	if !showVersion {
		return false
	}
	fmt.Fprint(os.Stdout, Report(program))
	return true
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none recorded)"
	}
	return s
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
