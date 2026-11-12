package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreditPoints_SingleCaller verifies CLM-9: in non-test service code,
// AddMemberPoints must only be referenced inside creditPoints.
func TestCreditPoints_SingleCaller(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}

	var foundInCreditPoints bool
	var violations []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		path := filepath.Clean(entry.Name())
		node, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for _, decl := range node.Decls {
			fnDecl, ok := decl.(*ast.FuncDecl)
			if !ok {
				ast.Inspect(decl, func(n ast.Node) bool {
					if ident, ok := n.(*ast.Ident); ok && ident.Name == "AddMemberPoints" {
						pos := fset.Position(n.Pos())
						violations = append(violations, fmt.Sprintf("%s (outside func)", pos))
					}
					return true
				})
				continue
			}

			funcName := fnDecl.Name.Name
			if fnDecl.Body == nil {
				continue
			}
			ast.Inspect(fnDecl.Body, func(n ast.Node) bool {
				if ident, ok := n.(*ast.Ident); ok && ident.Name == "AddMemberPoints" {
					if funcName == "creditPoints" {
						foundInCreditPoints = true
					} else {
						pos := fset.Position(n.Pos())
						violations = append(violations, fmt.Sprintf("%s (in func %q)", pos, funcName))
					}
				}
				return true
			})
		}
	}

	if !foundInCreditPoints {
		t.Fatal("expected AddMemberPoints to be called inside creditPoints, but none found")
	}

	if len(violations) > 0 {
		t.Errorf("AddMemberPoints referenced outside creditPoints in non-test service code:\n%s", strings.Join(violations, "\n"))
	}
}
