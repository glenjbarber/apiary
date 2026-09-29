package raft

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The FSM's apply path has to be deterministic. Every replica applies the
// same committed log entries and has to reach the same state, and a
// replay after a restart or a snapshot catch-up has to reach exactly
// what the node that applied the entry live reached. The wall clock is
// the one input a replica cannot reproduce, which is why a read of it
// anywhere on this path is a defect rather than a detail: it is exactly
// what shipped as finding A15 in
// docs/audits/2026-09-26-code-audit.md, when
// applyResolvePendingJoinRequest called time.Now() to decide whether a
// join request had expired, so replaying an approve, reject or cancel
// after the 15 minute TTL returned "expired" and left the request
// Pending on the replica that replayed it, while the node that applied
// it live had it Approved. Expiry moved to internal/manager, which
// checks the clock before submitting the command, and the apply path no
// longer reads it.
//
// TestFSMApplyPathNeverReadsTheWallClock is the guard against that
// coming back. It is a source-level reachability check rather than a
// runtime one on purpose: a runtime check can only catch the clock read
// that some test happens to drive through it, and the entries that
// matter most here - a replay of an old command, a branch only a
// failover takes - are precisely the ones no runtime test drives.
//
// The two halves of it are the reachability and the clock check, and
// they need each other. A clock check alone would have to name the
// functions to check, and half the apply path is reached through helpers
// that the read path shares (cloneColonyJoinWindow, snapshotStateLocked
// and the rest), so a name list would either miss the apply-only
// helpers' callers or pull in the read path and fail on
// pendingJoinRequestExpired, which does read the clock and is supposed
// to. So the set is derived by walking the call graph from the entry
// points, and the walk is why the check can be exact: a function is on
// the apply path if and only if the entry points reach it.

// fsmClockFuncs are the functions of the standard library's time
// package whose result depends on when they were called rather than on
// their arguments alone. Now reads the clock, Since and Until are
// defined in terms of it, and the timer constructors all schedule
// against the same runtime clock.
//
// The rest of the package is absent deliberately, and it is worth being
// explicit about which: Unix, Date, Parse and ParseDuration are pure
// functions of their arguments, and Duration and Time are types. An
// apply path that builds a Time out of a timestamp the log already
// carried is the intended shape, not a violation - pinned_at_unix and
// expires_at_unix exist so that the leader reads the clock once and
// every replica reads the timestamp.
var fsmClockFuncs = map[string]bool{
	"Now":       true,
	"Since":     true,
	"Until":     true,
	"Sleep":     true,
	"After":     true,
	"AfterFunc": true,
	"NewTimer":  true,
	"NewTicker": true,
	"Tick":      true,
}

// fsmApplyPathSeeds are the entry points raft itself drives the FSM
// through: the three methods of hashicorp/raft's raft.FSM, plus the two
// methods of the snapshot value Snapshot hands back, which raft calls on
// it afterwards and which are as much a part of the snapshot path as
// Snapshot is. If hashicorp/raft grows the FSM interface, this list has
// to grow with it - the test fails on a seed that no longer exists
// rather than quietly checking less than it used to.
var fsmApplyPathSeeds = []string{
	"FSM.Apply",
	"FSM.Snapshot",
	"FSM.Restore",
	"fsmSnapshot.Persist",
	"fsmSnapshot.Release",
}

// fsmFunc is one function or method of the package as parsed from source
// rather than as compiled, so the guard sees every call the compiler
// would see and nothing that is only a comment.
type fsmFunc struct {
	key      string // "FSM.Apply" for a method, "applyCreateVM" for a package-level function
	name     string // the bare name
	recvType string // the receiver's base type, "" for a package-level function
	recvName string // the receiver's identifier, "" for a package-level function
	imports  string // the name this file gives package time, "" if it does not import it
	pos      token.Position
	decl     *ast.FuncDecl
}

// fsmIndex is one parsed package: every function it declares, which of
// them share a bare name, and which package-level variables are bound to
// a clock read.
type fsmIndex struct {
	fset     *token.FileSet
	funcs    map[string]*fsmFunc
	byName   map[string][]string
	clockVar map[string]token.Position
}

// fsmFacts is what the guard needs to know about one function: which
// in-package functions it can call, whether it reads the wall clock
// itself, and whether it calls something on a receiver this walk cannot
// type.
type fsmFacts struct {
	next       []string
	clock      []string
	unresolved []string
}

// fsmWalk is the result of walking the call graph from a set of seeds.
type fsmWalk struct {
	reachable  map[string]bool
	clock      []string
	unresolved []string
}

func newFSMIndex(fset *token.FileSet) *fsmIndex {
	return &fsmIndex{
		fset:     fset,
		funcs:    map[string]*fsmFunc{},
		byName:   map[string][]string{},
		clockVar: map[string]token.Position{},
	}
}

// addFile parses one file of the package into the index.
func (ix *fsmIndex) addFile(t *testing.T, path string, src []byte) {
	t.Helper()
	file, err := parser.ParseFile(ix.fset, path, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	timeName, dotted := fsmTimeImport(file)
	if dotted {
		t.Fatalf("%s dot-imports time. The guard matches a clock read by the name its file imports time under, and a dot import puts those calls in scope unqualified, so it cannot prove anything about that file. Import time normally.", path)
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			fn := &fsmFunc{
				name:     d.Name.Name,
				recvType: fsmRecvType(d.Recv),
				recvName: fsmRecvName(d.Recv),
				imports:  timeName,
				pos:      ix.fset.Position(d.Pos()),
				decl:     d,
			}
			fn.key = fn.name
			if fn.recvType != "" {
				fn.key = fn.recvType + "." + fn.name
			}
			if prev, dup := ix.funcs[fn.key]; dup {
				t.Fatalf("%s declares %s, already declared at %s", path, fn.key, prev.pos)
			}
			ix.funcs[fn.key] = fn
			ix.byName[fn.name] = append(ix.byName[fn.name], fn.key)
		case *ast.GenDecl:
			// A package-level var bound to a clock read is the same bug
			// one indirection away from an apply-path call, so it is
			// indexed here rather than missed.
			if d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || !ix.exprsReadClock(timeName, vs.Values) {
					continue
				}
				for _, name := range vs.Names {
					ix.clockVar[name.Name] = ix.fset.Position(name.Pos())
				}
			}
		}
	}
}

// walk derives the apply path from seeds by following in-package calls
// transitively, and collects the clock reads it finds on the way.
func (ix *fsmIndex) walk(seeds []string) fsmWalk {
	out := fsmWalk{reachable: map[string]bool{}}
	queue := append([]string(nil), seeds...)
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if out.reachable[key] {
			continue
		}
		out.reachable[key] = true
		fn, ok := ix.funcs[key]
		if !ok {
			continue
		}
		facts := ix.facts(fn)
		out.clock = append(out.clock, facts.clock...)
		out.unresolved = append(out.unresolved, facts.unresolved...)
		queue = append(queue, facts.next...)
	}
	return out
}

// facts reads one function's body. It walks the AST rather than the
// text, so a mention of time.Now() in a doc comment - of which this
// package has several, each one explaining why it is not called - is
// invisible to it, while a call is a node in the tree whether or not
// anyone ever runs it.
func (ix *fsmIndex) facts(fn *fsmFunc) fsmFacts {
	var out fsmFacts
	seenNext := map[string]bool{}
	seenClock := map[string]bool{}
	addClock := func(s string) {
		if !seenClock[s] {
			seenClock[s] = true
			out.clock = append(out.clock, s)
		}
	}
	ast.Inspect(fn.decl, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if fn.imports == "" {
				return true
			}
			if id, ok := node.X.(*ast.Ident); !ok || id.Name != fn.imports {
				return true
			}
			if fsmClockFuncs[node.Sel.Name] {
				addClock(fmt.Sprintf("%s: %s.%s reads the wall clock", ix.fset.Position(node.Sel.Pos()), fn.imports, node.Sel.Name))
			}
		case *ast.Ident:
			// Any mention of such a var, not only a call through it.
			if pos, ok := ix.clockVar[node.Name]; ok {
				addClock(fmt.Sprintf("%s: %s is a package-level var bound to a clock read declared at %s", ix.fset.Position(node.Pos()), node.Name, pos))
			}
		case *ast.CallExpr:
			next, ambiguous := ix.resolve(fn, node.Fun)
			for _, key := range next {
				if seenNext[key] {
					continue
				}
				seenNext[key] = true
				out.next = append(out.next, key)
			}
			if ambiguous != "" {
				out.unresolved = append(out.unresolved, fmt.Sprintf("%s: %s(...)", ix.fset.Position(node.Fun.Pos()), ambiguous))
			}
		}
		return true
	})
	return out
}

// resolve maps the callee of one call expression to the in-package
// functions it can be, and names the callee when it cannot be mapped and
// the name is one the FSM itself answers to.
//
// A call on the enclosing function's own receiver resolves to that
// receiver type's method, which is every in-package method call on this
// path; a bare identifier resolves to a package-level function of that
// name. Anything else is a call on a value whose type this walk does not
// resolve, and for those the bare name is followed only when exactly one
// function in the package has it. That is a deliberate refusal to guess:
// a call this guard cannot place is a call whose body it has not read,
// and saying so is the honest outcome rather than passing on the chance
// that it was somebody else's method.
func (ix *fsmIndex) resolve(fn *fsmFunc, fun ast.Expr) (next []string, ambiguous string) {
	switch e := fun.(type) {
	case *ast.IndexExpr: // a generic instantiation, f[T](x)
		return ix.resolve(fn, e.X)
	case *ast.IndexListExpr:
		return ix.resolve(fn, e.X)
	case *ast.Ident:
		if target, ok := ix.funcs[e.Name]; ok && target.recvType == "" {
			return []string{target.key}, ""
		}
		return nil, ""
	case *ast.SelectorExpr:
		recv, ok := e.X.(*ast.Ident)
		if fn.recvType != "" && ok && recv.Name == fn.recvName {
			if _, ok := ix.funcs[fn.recvType+"."+e.Sel.Name]; ok {
				return []string{fn.recvType + "." + e.Sel.Name}, ""
			}
			return nil, ""
		}
		candidates := ix.byName[e.Sel.Name]
		if len(candidates) == 1 {
			return candidates, ""
		}
		for _, key := range candidates {
			if ix.funcs[key].recvType == "FSM" {
				return nil, e.Sel.Name
			}
		}
		return nil, ""
	}
	return nil, ""
}

// exprsReadClock reports whether any of exprs names a clock read
// through the name its file imports package time under.
func (ix *fsmIndex) exprsReadClock(timeName string, exprs []ast.Expr) bool {
	if timeName == "" {
		return false
	}
	found := false
	for _, expr := range exprs {
		ast.Inspect(expr, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == timeName && fsmClockFuncs[sel.Sel.Name] {
				found = true
			}
			return true
		})
	}
	return found
}

// fsmTimeImport reports the name a file imports package time under, and
// whether it dot-imports it.
func fsmTimeImport(file *ast.File) (local string, dotted bool) {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "time" {
			continue
		}
		if imp.Name == nil {
			return "time", false
		}
		if imp.Name.Name == "." {
			return "", true
		}
		return imp.Name.Name, false
	}
	return "", false
}

func fsmRecvType(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

func fsmRecvName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 || len(recv.List[0].Names) == 0 {
		return ""
	}
	return recv.List[0].Names[0].Name
}

func TestFSMApplyPathNeverReadsTheWallClock(t *testing.T) {
	ix := newFSMIndex(token.NewFileSet())
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		ix.addFile(t, name, src)
		parsed++
	}
	if parsed == 0 {
		t.Fatal("no non-test Go file next to this test: the guard proved nothing about the apply path")
	}
	for _, seed := range fsmApplyPathSeeds {
		if _, ok := ix.funcs[seed]; !ok {
			t.Fatalf("apply path entry point %s does not exist. fsmApplyPathSeeds names raft's entry points into the FSM and has to be updated with them.", seed)
		}
	}

	got := ix.walk(fsmApplyPathSeeds)

	// A call this guard could not place is a call whose body it has not
	// read, so it is reported rather than passed over.
	if len(got.unresolved) > 0 {
		sort.Strings(got.unresolved)
		t.Errorf("apply path calls whose receiver this walk cannot type, and whose name is one the FSM answers to. Refusing to pass, because one of these may be an apply-path method going unread:\n  %s", strings.Join(got.unresolved, "\n  "))
	}

	// A walk that reached nothing would satisfy the clock check below
	// trivially, so the functions the apply path is known to contain are
	// named here. Each is reached from Apply's command switch or from
	// another entry point, and the last three are reached across a file
	// boundary rather than from fsm.go.
	for _, want := range []string{
		"FSM.applyCreateVM",
		"FSM.applyCreateJail",
		"FSM.applyRecordJoinApproval",
		"FSM.applyResolvePendingJoinRequest",
		"FSM.applyPinTrustedPeer",
		"FSM.applyOpenColonyJoinWindow",
		"FSM.snapshotStateLocked",
		"FSM.recomputeStateDigestLocked",
		"cloneColonyJoinWindow",
		"evictSettledColonyUpdates",
		"ValidateJoinRequestFields",
	} {
		if !got.reachable[want] {
			t.Errorf("%s is not in the derived apply path, so the call graph is not being followed and the clock check below is not checking the whole path", want)
		}
	}

	// The read path shares helpers with the apply path, which is why this
	// guard separates call paths instead of matching names.
	// pendingJoinRequestExpired is the function that read the clock, and
	// the whole content of finding A15 was that it came off the apply
	// path: it must still be declared, and it must not be reachable from
	// any entry point.
	for _, readOnly := range []string{"pendingJoinRequestExpired", "FSM.ListPendingJoinRequests"} {
		if _, ok := ix.funcs[readOnly]; !ok {
			t.Errorf("%s is not declared any more, so the reachability assertion on it below needs replacing with whatever the read path reads the clock through now", readOnly)
		}
		if got.reachable[readOnly] {
			t.Errorf("%s is reachable from the apply path, so the apply path reads the wall clock", readOnly)
		}
	}

	if len(got.clock) > 0 {
		sort.Strings(got.clock)
		t.Errorf("the apply path reads the wall clock, so a replay after a restart or a snapshot catch-up can reach a different state from the node that applied the entry live:\n  %s", strings.Join(got.clock, "\n  "))
	}

	// Reachability and the clock check are the two halves of this guard,
	// and a guard that cannot fail is not a guard. Both are therefore run
	// again over packages written for the purpose: one where the clock
	// read is only on the read path, and one where it is on the apply
	// path, directly and through a package-level var.
	readOnlyFixture := "package raft\n\nimport \"time\"\n\n" +
		"func apply() int    { return shared() }\n" +
		"func read() int     { return readOnly() }\n" +
		"func shared() int   { return 1 }\n" +
		"func readOnly() int { return int(time.Now().Unix()) }\n"
	ro := newFSMIndex(token.NewFileSet())
	ro.addFile(t, "fixture.go", []byte(readOnlyFixture))
	if walk := ro.walk([]string{"apply"}); len(walk.clock) != 0 {
		t.Errorf("the clock read above is reachable only from the read path, and walking from apply reported it anyway: %v", walk.clock)
	}
	if walk := ro.walk([]string{"read"}); len(walk.clock) != 1 {
		t.Errorf("walking from the read function found %d clock reads, want exactly the 1 planted at readOnly: %v", len(walk.clock), walk.clock)
	}

	applyFixture := "package raft\n\nimport \"time\"\n\n" +
		"func apply() int  { return shared() }\n" +
		"func shared() int { return int(time.Since(deadline).Seconds()) }\n"
	ap := newFSMIndex(token.NewFileSet())
	ap.addFile(t, "fixture.go", []byte(applyFixture))
	if walk := ap.walk([]string{"apply"}); len(walk.clock) != 1 {
		t.Errorf("a time.Since() on the apply path reported %d clock reads, want exactly 1: %v", len(walk.clock), walk.clock)
	}

	varFixture := "package raft\n\nimport \"time\"\n\nvar applyClock = time.Now\n\nfunc apply() int { return int(applyClock().Unix()) }\n"
	vr := newFSMIndex(token.NewFileSet())
	vr.addFile(t, "fixture.go", []byte(varFixture))
	if walk := vr.walk([]string{"apply"}); len(walk.clock) == 0 {
		t.Error("a package-level var bound to time.Now, called from the apply path, was not reported")
	}
}
