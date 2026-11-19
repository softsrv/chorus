package http_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxLines is how long a hand-written Go file in this package, or a test
// function body, may grow before it can no longer be read whole.
const maxLines = 1000

// TestLayout_FilesAndTestsReadWhole: every Go file in internal/http, and every
// Test function body in it, stays within maxLines lines.
func TestLayout_FilesAndTestsReadWhole(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list Go files: %v (%d files)", err, len(files))
	}
	fset := token.NewFileSet()
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if n := strings.Count(string(src), "\n"); n > maxLines {
			t.Errorf("%s is %d lines, want at most %d", path, n, maxLines)
		}
		// The parser ignores build tags, so tagged files are checked too.
		node, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range node.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			body := fset.Position(fn.Body.Rbrace).Line - fset.Position(fn.Body.Lbrace).Line - 1
			if body > maxLines {
				t.Errorf("%s: %s body is %d lines, want at most %d", path, fn.Name.Name, body, maxLines)
			}
		}
	}
}

// TestLayout_IntegrationFilesTagged: every *_integration_test.go file starts
// with the integration build tag and is in package http_test, so the untagged
// build and vet skip it and the tagged ones include it.
func TestLayout_IntegrationFilesTagged(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*_integration_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list integration test files: %v (%d files)", err, len(files))
	}
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.SplitN(string(src), "\n", 4)
		if len(lines) < 3 || lines[0] != "//go:build integration" || lines[1] != "" || lines[2] != "package http_test" {
			t.Errorf("%s does not start with //go:build integration, a blank line and package http_test", path)
		}
	}
}
