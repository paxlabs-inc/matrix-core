package math

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoFloatsInPerpsDomain is the source audit required by the fixed-point
// law: no float32/float64 identifier may appear in any non-test source file of
// the perps domain (internal/perps/... and the Crossverse market-data adapter).
func TestNoFloatsInPerpsDomain(t *testing.T) {
	roots := []string{"..", "../../marketdata"}
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				ident, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if ident.Name == "float32" || ident.Name == "float64" {
					t.Errorf("%s: %s used at %s", path, ident.Name, fset.Position(ident.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
