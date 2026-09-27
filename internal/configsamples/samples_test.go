// Package configsamples holds the tests that keep the shipped
// configuration samples honest.
//
// The samples in etc/apiary/*.sample are installed onto every node by
// `make install` and are what an operator copies verbatim to
// /usr/local/etc/apiary/<name>.json. For most of their life they were
// heavy with `//` commentary, and JSON has no comment syntax: both
// encoding/json and internal/jsonstrict reject the file, so the copy
// that every bootstrap guide instructs an operator to make would not
// parse. The guidance that used to live in those comments now lives in
// etc/apiary/README.md, and the samples are plain, comment-free,
// strict-parseable JSON.
//
// This is a test-only package, because the property under test belongs
// to no daemon: it is a property of files that sit on disk next to the
// real config, which no daemon ever reads.
package configsamples

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/jsonstrict"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
)

// sampleDir is etc/apiary, resolved from this test file's own compiled-in
// path rather than from the working directory. `go test ./...` sets the
// working directory to the package's own directory, but a bare `go
// test` of a package list, an editor's test runner, or a test binary
// invoked by hand may not, and a test that silently found no samples
// because it looked in the wrong place would pass without testing
// anything.
func sampleDir(t *testing.T) string {
	t.Helper()
	// internal/configsamples -> internal -> <module root>.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate this test file, so the sample directory cannot be resolved")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	return filepath.Join(root, "etc", "apiary")
}

// samples enumerates every shipped sample, sorted, with no hardcoded
// list: a fifth daemon that ships a sample is covered by every test here
// the moment its file exists, with nothing to remember to add. A sample
// file that does not exist cannot be caught this way, which is what
// TestEverySampleHasALoaderCase exists for.
func samples(t *testing.T) []string {
	t.Helper()
	dir := sampleDir(t)
	paths, err := filepath.Glob(filepath.Join(dir, "*.sample"))
	if err != nil {
		t.Fatalf("Glob(%s/*.sample) error: %v", dir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no *.sample files found in %s: the glob found nothing, so every check below would pass vacuously", dir)
	}
	slices.Sort(paths)
	return paths
}

// installedAs is the path an operator ends up with after copying a
// sample to its real home, named in error messages so a failure points
// at the file that will actually be read on a node rather than only at
// the template.
func installedAs(sample string) string {
	return filepath.Join("/usr/local/etc/apiary", strings.TrimSuffix(sample, ".sample"))
}

// TestSamplesAreValidStrictJSON is the test that would have caught the
// original defect. Every sample, exactly as it is shipped and exactly as
// `cp` puts it on a node, must parse with internal/jsonstrict, the
// strictest reader in this codebase, and not merely with some more
// forgiving hand-rolled pass.
func TestSamplesAreValidStrictJSON(t *testing.T) {
	for _, path := range samples(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s) error: %v", path, err)
			}

			// Nothing is stripped, nothing is normalised, no
			// comment-stripping pre-parser is applied. An operator
			// copies this file byte for byte, so the bytes are what
			// has to parse.
			var doc map[string]any
			if err := jsonstrict.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("%s does not parse as strict JSON as shipped.\n"+
					"An operator who copies this file verbatim to %s gets exactly this error from the daemon, and nothing in the file tells them why.\n"+
					"Put the explanation in etc/apiary/README.md and leave the sample comment-free.\n\n"+
					"underlying error: %v", path, installedAs(filepath.Base(path)), err)
			}

			assertNoCommentMarkers(t, path, raw)
		})
	}
}

// assertNoCommentMarkers names the failure mode directly. The strict
// parse above already catches a comment, but it catches it as
// "invalid character '/' looking for beginning of object key string",
// which reads like a typo to whoever hits it and does not say that a
// comment got back into a sample file. The markers are looked for
// outside JSON string literals only: a value that legitimately contains
// "//" is a value, not a comment, and flagging it would push the next
// author toward a workaround rather than toward the fix.
func assertNoCommentMarkers(t *testing.T, path string, raw []byte) {
	t.Helper()
	inString := false
	escaped := false
	line := 1
	for i := 0; i < len(raw) && i+1 < len(raw); i++ {
		c := raw[i]
		if c == '\n' {
			line++
			inString = false // an unescaped newline cannot occur inside a JSON string
			escaped = false
			continue
		}
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if c != '/' {
			continue
		}
		marker := ""
		switch raw[i+1] {
		case '/':
			marker = "//"
		case '*':
			marker = "/*"
		}
		if marker == "" {
			continue
		}
		t.Errorf("%s line %d contains a %q comment marker.\n"+
			"JSON has no comment syntax and neither encoding/json nor internal/jsonstrict accepts one, so a sample carrying comments cannot be copied to %s and loaded.\n"+
			"Move the prose to etc/apiary/README.md (or the field's own doc comment in the matching internal/*config package) and delete the comment.",
			path, line, marker, installedAs(filepath.Base(path)))
		return // one report per file is enough to send someone to the right place
	}
}

// loader is one sample plus the real config loader that will read it.
//
// load is the daemon's own Manager.Load, not a re-implementation: the
// property under test is that a copy of the sample starts the daemon,
// and only a call into the production loader can say so. It is handed a
// real path in a real directory, so everything downstream of parsing
// runs exactly as it does on a node: Validate, the defaults() overlay,
// the sibling common.json lookup, warnIfWorldReadable.
type loader struct {
	// file is the sample's basename, e.g. "managerd.json.sample".
	file string

	// subs maps each placeholder the sample ships to a value an
	// operator would put there. The samples deliberately carry
	// placeholders ("<this-node-hostname>") rather than realistic-looking
	// values, because a plausible-looking value is one an operator
	// forgets to edit, and a raft node once committed the literal
	// placeholder "<this-node-id>" into raft state for good. Substituting
	// here is the honest stand-in for the editing a real operator does
	// before the file goes live. Every value is fabricated:
	// node1.example.lab and 192.0.2.10 are documentation and test
	// values and name no real host.
	subs map[string]string

	// cfgType is the loader's Config type, used to check that the
	// sample and the struct agree about which fields exist.
	cfgType reflect.Type

	// load reads path and returns the loaded Config as any.
	load func(path string) (any, error)

	// spot pins the values a reader would most likely mistake for a
	// typo if they changed: the two addresses the whole bind-versus-dial
	// design turns on, and the paths raftd and managerd must agree on.
	// Asserted against the loaded Config, never against any defaults().
	spot map[string]any
}

// loaders is the one place a sample is named. Every other test here
// enumerates the directory instead, so a new sample is picked up
// automatically; this table exists because a sample cannot be loaded
// generically, and TestEverySampleHasALoaderCase is what keeps the table
// from falling behind the directory.
func loaders() []loader {
	return []loader{
		{
			file: "managerd.json.sample",
			subs: map[string]string{
				"<this-node-id>":       "node1",
				"<this-node-hostname>": "node1.example.lab",
				"<uplink-ifname>":      "igb0",
				"<your-pool-name>":     "tank",
			},
			cfgType: reflect.TypeOf(nodeconfig.Config{}),
			load: func(path string) (any, error) {
				m := nodeconfig.Manager{Path: path}
				return m.Load()
			},
			spot: map[string]any{
				// Names the node's own DNS name, which is the one
				// host its certificate carries a DNS SAN for, and is
				// simultaneously a bind address and a dial target:
				// never 0.0.0.0, which raftd's confirmation hook
				// would be refused by (ADR-0125, ADR-0139).
				"rpc_addr": "node1.example.lab:17700",
				// Must match raftd.json.sample's socket exactly, or
				// managerd cannot reach its own raft node.
				"raftd_socket": "/var/run/apiary/raftd.sock",
				"tls_cert":     "/usr/local/etc/apiary-tls/cert.pem",
				"tls_key":      "/usr/local/etc/apiary-tls/key.pem",
				// Tri-state fields ship as explicit false, which is a
				// local override rather than "unset"; keep them
				// spelled so a reader knows the sample means it.
				"peer_tls":     false,
				"hast_enabled": false,
				"bhyve_prefix": "",
			},
		},
		{
			file: "raftd.json.sample",
			subs: map[string]string{
				"<this-node-id>":      "node1",
				"<this-host-address>": "192.0.2.10",
			},
			cfgType: reflect.TypeOf(raftdconfig.Config{}),
			load: func(path string) (any, error) {
				m := raftdconfig.Manager{Path: path}
				return m.Load()
			},
			spot: map[string]any{
				// Advertised to Raft peers, which must be able to
				// dial it, so never 0.0.0.0.
				"raft_bind": "192.0.2.10:17600",
				"socket":    "/var/run/apiary/raftd.sock",
				"data_dir":  "/var/db/apiary/raftd",
				// await_join is the current safe multi-node flow
				// (ADR-0083); the shipped default stays false, since
				// true here is how a first node silently refuses to
				// self-bootstrap.
				"await_join": false,
			},
		},
		{
			file: "frontend.json.sample",
			subs: map[string]string{
				"<this-node-hostname>": "node1.example.lab",
			},
			cfgType: reflect.TypeOf(frontendconfig.Config{}),
			load: func(path string) (any, error) {
				m := frontendconfig.Manager{Path: path}
				return m.Load()
			},
			spot: map[string]any{
				// 0.0.0.0 here is DELIBERATE: the web UI is the
				// operator's browser-facing surface and is meant to
				// be reachable from the LAN. Note that this differs
				// from frontendconfig.defaults()' own 127.0.0.1:8080,
				// and from restshimd's sample; do not "fix" it.
				"http_addr": "0.0.0.0:8080",
				// Must be managerd.json.sample's rpc_addr exactly,
				// hostname and all (ADR-0139).
				"manager_addr": "node1.example.lab:17700",
				"manager_tls":  true,
			},
		},
		{
			file: "restshimd.json.sample",
			subs: map[string]string{
				"<this-node-hostname>": "node1.example.lab",
			},
			cfgType: reflect.TypeOf(restshimdconfig.Config{}),
			load: func(path string) (any, error) {
				m := restshimdconfig.Manager{Path: path}
				return m.Load()
			},
			spot: map[string]any{
				// Loopback only, unlike frontend's 0.0.0.0:8080,
				// and on purpose: nothing inside the Colony dials
				// this, and it has no authentication of its own.
				"http_addr": "127.0.0.1:8081",
				// Must be managerd.json.sample's rpc_addr exactly,
				// hostname and all (ADR-0139).
				"manager_addr": "node1.example.lab:17700",
				"manager_tls":  true,
			},
		},
	}
}

// TestEverySampleHasALoaderCase keeps the table above from falling
// behind the directory. A sample that reached etc/apiary without anyone
// adding it to loaders() would be parsed but never loaded, and the one
// check that proves a copy of it actually starts a daemon would silently
// not be running.
func TestEverySampleHasALoaderCase(t *testing.T) {
	found := map[string]bool{}
	for _, path := range samples(t) {
		found[filepath.Base(path)] = true
	}
	covered := map[string]bool{}
	for _, l := range loaders() {
		if covered[l.file] {
			t.Errorf("loaders() names %s twice", l.file)
		}
		covered[l.file] = true
		if !found[l.file] {
			t.Errorf("loaders() names %s, which is not a sample in %s: a loader case for a file that does not exist", l.file, sampleDir(t))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if !covered[name] {
			t.Errorf("%s has no loader case in loaders(): it is parsed by TestSamplesAreValidStrictJSON but never actually loaded through a real config loader. Add an entry.", name)
		}
	}
}

// TestSampleLoadsThroughItsRealLoader copies each sample into a temp
// directory under the name its daemon really reads, substitutes the
// operator's placeholders, and calls the production loader.
//
// The loaders are the real ones, so their own checks run too:
// frontendconfig and restshimdconfig both call Config.Validate, and a
// sample whose manager_addr were a wildcard, or whose tls_cert were set
// without a tls_key, would fail here rather than on a node.
//
// On defaults: nothing here compares a loaded Config against a
// defaults()-derived expectation, because three of the four samples
// deliberately DISAGREE with their loader's defaults. frontend's sample
// says 0.0.0.0:8080 where defaults() says 127.0.0.1:8080; managerd's
// and raftd's name the node where the defaults are loopback. A
// comparison built from defaults() reports those three as differences,
// which is exactly the spurious difference a reference-file test
// produces when it forgets to start from the loader's own defaults: the
// fields the file does not mention get filled in underneath it, and the
// test then blames the file for them. So the field-set check below
// compares keys, which defaults cannot change, and the value check pins
// exactly the values the sample is meant to carry, spelled out per
// sample rather than derived.
func TestSampleLoadsThroughItsRealLoader(t *testing.T) {
	for _, l := range loaders() {
		t.Run(l.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(sampleDir(t), l.file))
			if err != nil {
				t.Fatalf("ReadFile(%s) error: %v", l.file, err)
			}

			// The real config path, in a real directory, so a
			// loader that reads a sibling file (common.json) sees
			// what it would see on a node.
			dir := t.TempDir()
			path := filepath.Join(dir, strings.TrimSuffix(l.file, ".sample"))
			body := substitute(t, l, raw)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatalf("WriteFile(%s) error: %v", path, err)
			}

			loaded, err := l.load(path)
			if err != nil {
				t.Fatalf("%s does not load through its real config loader after placeholder substitution: %v\n"+
					"Whatever an operator has to change beyond the placeholders, it is a defect: a sample is meant to be a starting point that runs.", l.file, err)
			}

			// The substituted bytes must still be strict JSON, and
			// they are re-read from disk rather than reused from
			// memory, so the file the loader saw is the file the
			// key check below is talking about.
			onDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s) error: %v", path, err)
			}
			var doc map[string]any
			if err := jsonstrict.Unmarshal(onDisk, &doc); err != nil {
				t.Fatalf("the substituted %s does not parse as strict JSON: %v", l.file, err)
			}

			// The sample and the struct must agree about which
			// fields exist, in both directions. A key the struct
			// does not have is a typo that encoding/json drops in
			// silence: the operator sets it, believes it is set,
			// and it never reaches the daemon. A field the sample
			// omits is a setting with no working example, which is
			// how a sample becomes authoritative in the first
			// place.
			known := jsonFields(l.cfgType)
			for _, key := range slices.Sorted(maps.Keys(doc)) {
				if !slices.Contains(known, key) {
					t.Errorf("%s sets %q, which is not a field of %s: encoding/json drops it silently, so the operator's edit would never reach the daemon",
						l.file, key, l.cfgType)
				}
			}
			for _, key := range known {
				if _, ok := doc[key]; !ok {
					t.Errorf("%s omits %q, a field %s does have: the sample is the reference an operator copies, so a field with no example here is a field with no documented value",
						l.file, key, l.cfgType)
				}
			}

			for _, key := range slices.Sorted(maps.Keys(l.spot)) {
				got := fieldByJSONName(reflect.ValueOf(loaded), key)
				if !got.IsValid() {
					t.Errorf("%s: spot check %q is not a field of %s", l.file, key, l.cfgType)
					continue
				}
				if want := l.spot[key]; !reflect.DeepEqual(deref(got.Interface()), want) {
					t.Errorf("%s: %s = %#v after loading, want %#v", l.file, key, deref(got.Interface()), want)
				}
			}
		})
	}
}

// substitute applies the operator's placeholder edits and then insists
// that none is left. A placeholder spelled differently in the sample
// than in the table above would otherwise sail through, leaving the
// daemon to be handed a literal "<this-node-hostname>" at startup.
func substitute(t *testing.T, l loader, raw []byte) []byte {
	t.Helper()
	body := string(raw)
	for placeholder, value := range l.subs {
		if !strings.Contains(body, placeholder) {
			t.Errorf("%s contains no %q, so this loader's placeholder substitution is stale", l.file, placeholder)
		}
		body = strings.ReplaceAll(body, placeholder, value)
	}
	if i := strings.IndexByte(body, '<'); i >= 0 {
		rest := body[i:]
		if j := strings.IndexByte(rest, '>'); j >= 0 {
			t.Errorf("%s still holds the unsubstituted placeholder %q after substitution: add it to this loader's subs", l.file, rest[:j+1])
		}
	}
	return []byte(body)
}

// jsonFields returns the JSON object keys a config struct can hold,
// ignoring Go-side implementation details. The fields of an embedded
// struct with no tag are followed, so a promoted field is still found.
func jsonFields(t reflect.Type) []string {
	seen := map[string]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag, ok := f.Tag.Lookup("json")
			if !ok {
				if f.Anonymous && f.Type.Kind() == reflect.Struct {
					walk(f.Type)
				}
				continue
			}
			if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
				seen[name] = true
			}
		}
	}
	walk(t)
	return slices.Sorted(maps.Keys(seen))
}

// deref collapses a pointer to the value it points at. Several config
// fields are *bool or *string precisely so that "unset" and "explicitly
// false/empty" are distinguishable - a sample that ships an explicit
// false therefore loads as a non-nil pointer to false, and comparing
// that against a bare `false` would fail for the wrong reason. The
// pointer-ness is checked separately by the "every field is present in
// the sample" pass, which only cares about the JSON name.
func deref(v any) any {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil
	}
	return rv.Interface()
}

// fieldByJSONName reads a loaded Config by its JSON name, so the checks
// above name the same thing the file names.
func fieldByJSONName(v reflect.Value, name string) reflect.Value {
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, ok := f.Tag.Lookup("json")
		if !ok {
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				if got := fieldByJSONName(v.Field(i), name); got.IsValid() {
					return got
				}
			}
			continue
		}
		if key, _, _ := strings.Cut(tag, ","); key == name && key != "-" {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

// TestEveryFileInEtcApiaryIsInstalled guards the seam between this
// directory and `make install`. The install rule copies samples by
// looping over INSTALL_SRCS and naming each one, so a file added here
// that no rule names is silently never shipped: it exists in the
// repository and on no node at all. That is not hypothetical - the
// first version of this directory's README was exactly that, because
// the samples became bare JSON and the guidance moved out of them while
// the install rule still only knew about *.json.sample. An operator on
// a node then had a sample with no documentation beside it.
//
// The check is deliberately coarse: it requires each file in this
// directory to be named somewhere in the Makefile. It does not prove
// the rule is correct, only that nothing here is forgotten, which is
// the failure that actually happened and the one a new file invites.
func TestEveryFileInEtcApiaryIsInstalled(t *testing.T) {
	dir := sampleDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error: %v", dir, err)
	}

	// sampleDir is <root>/etc/apiary, so the module root is two levels up.
	root := filepath.Dir(filepath.Dir(dir))
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading the Makefile to check it installs %s: %v", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if isInstalled(makefile, name) {
			continue
		}
		t.Errorf("%s/%s is not named in the Makefile, so `make install` never puts it on a node.\n"+
			"Add it to the install rule, or move it out of this directory if it is not meant to ship.",
			dir, name)
	}
}

// isInstalled reports whether the Makefile puts name on a node, either by
// naming it literally or by covering it through the templated loop that
// copies every daemon's sample (`for S in ${INSTALL_SRCS} ... $$S.json.sample`).
// A per-daemon sample is never named literally, so a literal-only check
// reports all four as missing and the test is worthless.
func isInstalled(makefile []byte, name string) bool {
	haystack := string(makefile)
	if strings.Contains(haystack, "etc/apiary/"+name) {
		return true
	}
	if daemon, ok := strings.CutSuffix(name, ".json.sample"); ok && daemon != "" {
		// The loop writes etc/apiary/<daemon>.json.sample from a
		// template, so look for that template with the daemon's own
		// name replaced by the loop variable.
		if strings.Contains(haystack, "$$S.json.sample") || strings.Contains(haystack, "${INSTALL_SRCS}.json.sample") {
			return true
		}
	}
	return false
}
