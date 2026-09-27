package tlsdial

import (
	"crypto/tls"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every tls.Config literal in the daemons must state a MinVersion, so a
// future dial or listener cannot quietly rely on a library default. The one
// exception is internal/managerlink/scheme.go, whose config exists only to
// detect whether a peer speaks TLS at all: raising its minimum would make a
// legacy-only TLS peer look like plaintext, which is the wrong answer to the
// only question it asks (and it sends no data and trusts nothing).
func TestEveryTLSConfigLiteralSetsMinVersion(t *testing.T) {
	exempt := map[string]bool{filepath.ToSlash("internal/managerlink/scheme.go"): true}
	root := filepath.Join("..", "..")

	var missing []string
	fset := token.NewFileSet()
	for _, dir := range []string{"cmd", "internal"} {
		filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if exempt[rel] {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Config" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "tls" {
					return true
				}
				for _, el := range lit.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "MinVersion" {
							return true
						}
					}
				}
				missing = append(missing, rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line))
				return true
			})
			return nil
		})
	}
	if len(missing) > 0 {
		t.Errorf("tls.Config literals with no MinVersion (set tls.VersionTLS12 or higher): %v", missing)
	}
}

func TestManagerTLSConfig_RequiresTLS12OrNewer(t *testing.T) {
	cfg, err := ManagerTLSConfig(true, "", "example.internal")
	if err != nil {
		t.Fatalf("ManagerTLSConfig() error: %v", err)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want at least TLS 1.2", cfg.MinVersion)
	}
}
