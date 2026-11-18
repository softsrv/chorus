// Package archtest holds the part-boundary checks: which chorus packages may
// import which, and that the web client's JS stays inside web/. It is test-only
// (no non-test file), imports no chorus package and nothing imports it.
package archtest

import (
	"bufio"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// prodAllow is the production allowlist: for each chorus package (path
// relative to the module root), the chorus packages its non-test files may
// import. A package missing from this map may import no chorus package, and a
// package listed nowhere as a target may be imported by nobody (cmd/app,
// internal/archtest).
var prodAllow = map[string][]string{
	"cmd/app":                  {"internal/app", "internal/auth", "internal/db", "internal/email", "internal/http", "internal/http/handlers", "internal/http/middleware", "internal/http/respond", "internal/users"},
	"internal/http":            {"internal/http/handlers", "internal/http/middleware", "internal/http/respond", "internal/app", "internal/db", "web"},
	"internal/http/handlers":   {"internal/app", "internal/db", "internal/http/middleware", "internal/http/respond"},
	"internal/http/middleware": {"internal/auth", "internal/db", "internal/http/respond"},
	"internal/app":             {"internal/auth", "internal/db", "internal/email", "internal/users"},
	"internal/auth":            nil,
	"internal/users":           nil,
	"internal/email":           nil,
	"internal/http/respond":    nil,
	"internal/db":              nil,
	"web":                      nil,
	"internal/archtest":        nil,
	"internal/testnet":         nil,
	"systest":                  nil,
}

// testExtra is what a package's _test.go files (internal and external test
// packages) may import beyond the package under test and its production
// allowlist. internal/http's integration tests and internal/http/handlers'
// tests mint tokens with internal/auth;
// internal/app and internal/http tests refuse outbound dials with
// internal/testnet, a test-only helper no production file may import.
var testExtra = map[string][]string{
	"internal/http":          {"internal/auth", "internal/testnet"},
	"internal/app":           {"internal/testnet"},
	"internal/http/handlers": {"internal/auth"},
}

// edge is one chorus import: the importing file, its package directory, the
// imported package (both relative to the module root) and whether the file is
// a test file.
type edge struct {
	File string
	From string
	To   string
	Test bool
}

func allowed(list []string, to string) bool { return slices.Contains(list, to) }

// prodAllowed reports whether a non-test file of from may import to: cmd/app
// may import any internal/* package (the bootstrap wires them all); everyone
// else is held to prodAllow.
func prodAllowed(from, to string) bool {
	if from == "cmd/app" {
		return strings.HasPrefix(to, "internal/") && to != "internal/archtest" && to != "internal/testnet"
	}
	return allowed(prodAllow[from], to)
}

// checkEdges applies the production rules to non-test edges and the test rules
// to test edges, and returns one message per violation naming file and edge.
// It is pure: no filesystem access.
func checkEdges(edges []edge) []string {
	var out []string
	for _, e := range edges {
		bad := func(why string) {
			kind := "production"
			if e.Test {
				kind = "test"
			}
			out = append(out, fmt.Sprintf("%s: %s file imports %s -> %s: %s", e.File, kind, e.From, e.To, why))
		}
		switch {
		case e.To == "cmd/app":
			bad("nothing may import cmd/app")
		case e.To == "internal/archtest":
			bad("nothing may import internal/archtest")
		case !e.Test:
			if !prodAllowed(e.From, e.To) {
				bad(fmt.Sprintf("not in the production allowlist for %s %v", e.From, prodAllow[e.From]))
			}
		case (e.From == "web" || strings.HasPrefix(e.From, "web/")) && strings.HasPrefix(e.To, "internal/"):
			bad("web tests never import internal/*")
		case e.To == e.From:
			// a test of its own package (internal or external test package)
		case prodAllowed(e.From, e.To), allowed(testExtra[e.From], e.To):
		default:
			bad(fmt.Sprintf("tests of %s may import only it, %v and %v", e.From, prodAllow[e.From], testExtra[e.From]))
		}
	}
	return out
}

// moduleRoot walks up from the working directory to the directory holding
// go.mod and returns it with the module path.
func moduleRoot(t *testing.T) (string, string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if mod, err := modulePath(filepath.Join(dir, "go.mod")); err == nil {
			return dir, mod
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}

func modulePath(gomod string) (string, error) {
	f, err := os.Open(gomod)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s: no module line", gomod)
}

// scanEdges parses the imports of every .go file under root that builds with
// the given tags (filename and //go:build constraints both apply), test files
// included, and returns the edges into the module. A file is a test file when
// its name ends in _test.go, which covers internal and external test packages.
func scanEdges(root, module string, tags []string) ([]edge, error) {
	ctx := build.Default
	ctx.BuildTags = tags
	fset := token.NewFileSet()
	var edges []edge
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "node_modules" || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		dir := filepath.Dir(p)
		if ok, err := ctx.MatchFile(dir, name); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		} else if !ok {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		relDir, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		relFile, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		from := filepath.ToSlash(relDir)
		for _, imp := range f.Imports {
			ip, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			var to string
			switch {
			case ip == module:
				to = "."
			case strings.HasPrefix(ip, module+"/"):
				to = strings.TrimPrefix(ip, module+"/")
			default:
				continue
			}
			edges = append(edges, edge{File: filepath.ToSlash(relFile), From: from, To: to, Test: strings.HasSuffix(name, "_test.go")})
		}
		return nil
	})
	return edges, err
}

var tagSets = [][]string{nil, {"integration"}}

func TestImportBoundaries(t *testing.T) {
	root, module := moduleRoot(t)

	for _, tags := range tagSets {
		name := "tags=" + strings.Join(tags, ",")
		t.Run(name, func(t *testing.T) {
			edges, err := scanEdges(root, module, tags)
			if err != nil {
				t.Fatalf("scan imports: %v", err)
			}
			if len(edges) == 0 {
				t.Fatal("found no chorus import edges; the scan is not seeing the module")
			}
			for _, v := range checkEdges(edges) {
				t.Error(v)
			}
			for _, e := range edges {
				if e.From == "internal/archtest" || e.To == "internal/archtest" {
					t.Errorf("%s: internal/archtest must import no chorus package and have no importers (%s -> %s)", e.File, e.From, e.To)
				}
			}
		})
	}

	t.Run("archtest is test-only", func(t *testing.T) {
		entries, err := os.ReadDir(filepath.Join(root, "internal", "archtest"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
				t.Errorf("internal/archtest/%s: the package must hold only _test.go files", e.Name())
			}
		}
	})

	t.Run("detector fires: production rules", func(t *testing.T) {
		targets := knownPackages()
		for from := range prodAllow {
			for _, to := range targets {
				if to == from {
					continue
				}
				got := checkEdges([]edge{{File: from + "/x.go", From: from, To: to}})
				want := prodAllowed(from, to)
				if want && len(got) != 0 {
					t.Errorf("allowed production edge %s -> %s reported: %v", from, to, got)
				}
				if !want && len(got) != 1 {
					t.Errorf("forbidden production edge %s -> %s not reported once: %v", from, to, got)
				}
			}
		}
		for _, e := range []edge{
			{File: "internal/newpkg/x.go", From: "internal/newpkg", To: "internal/db"},
			{File: "internal/auth/x.go", From: "internal/auth", To: "internal/auth"},
			{File: "internal/app/x.go", From: "internal/app", To: "internal/archtest"},
		} {
			if got := checkEdges([]edge{e}); len(got) != 1 || !strings.Contains(got[0], e.File) {
				t.Errorf("forbidden production edge %+v not reported naming its file: %v", e, got)
			}
		}
	})

	t.Run("detector fires: test rules", func(t *testing.T) {
		targets := knownPackages()
		for from := range prodAllow {
			for _, to := range targets {
				want := to != "cmd/app" && to != "internal/archtest" &&
					!(from == "web" && strings.HasPrefix(to, "internal/")) &&
					(to == from || prodAllowed(from, to) || allowed(testExtra[from], to))
				got := checkEdges([]edge{{File: from + "/x_test.go", From: from, To: to, Test: true}})
				if want && len(got) != 0 {
					t.Errorf("allowed test edge %s -> %s reported: %v", from, to, got)
				}
				if !want && len(got) != 1 {
					t.Errorf("forbidden test edge %s -> %s not reported once: %v", from, to, got)
				}
			}
		}
		for _, c := range []struct {
			e    edge
			want bool
		}{
			{edge{"internal/http/a_integration_test.go", "internal/http", "internal/auth", true}, true},
			{edge{"internal/users/v_test.go", "internal/users", "internal/users", true}, true},
			{edge{"internal/http/handlers/h_test.go", "internal/http/handlers", "internal/email", true}, false},
			{edge{"internal/auth/a_test.go", "internal/auth", "internal/db", true}, false},
			{edge{"internal/app/a_test.go", "internal/app", "cmd/app", true}, false},
			{edge{"web/w_test.go", "web", "internal/db", true}, false},
			{edge{"internal/archtest/b_test.go", "internal/archtest", "internal/db", true}, false},
		} {
			got := checkEdges([]edge{c.e})
			if c.want != (len(got) == 0) {
				t.Errorf("test edge %+v: want allowed=%v, got %v", c.e, c.want, got)
			}
		}
	})

	t.Run("detector fires: integration-tagged and external test files are seen", func(t *testing.T) {
		dir := t.TempDir()
		write := func(rel, src string) {
			p := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("go.mod", "module "+module+"\n")
		write("internal/auth/bad.go", "//go:build integration\n\npackage auth\n\nimport _ \""+module+"/internal/db\"\n")
		write("internal/users/x_test.go", "package users_test\n\nimport _ \""+module+"/cmd/app\"\n")

		plain, err := scanEdges(dir, module, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := checkEdges(plain); len(got) != 1 || !strings.Contains(got[0], "internal/users/x_test.go") {
			t.Errorf("default tags: want only the external test edge to cmd/app flagged, got %v", got)
		}
		tagged, err := scanEdges(dir, module, []string{"integration"})
		if err != nil {
			t.Fatal(err)
		}
		got := checkEdges(tagged)
		sort.Strings(got)
		if len(got) != 2 || !strings.Contains(got[0], "internal/auth/bad.go") || !strings.Contains(got[0], "internal/auth -> internal/db") {
			t.Errorf("integration tag: want the tagged internal/auth -> internal/db edge flagged, got %v", got)
		}
	})
}

// knownPackages is every package the rules name, as source or target.
func knownPackages() []string {
	seen := map[string]bool{}
	for from, list := range prodAllow {
		seen[from] = true
		for _, to := range list {
			seen[to] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// jsSpecifier matches the specifier of a static import/export ... from,
// a side-effect import and a dynamic import().
var jsSpecifier = regexp.MustCompile(`(?:\bfrom\s*|\bimport\s*\(?\s*)(?:'([^'\n]*)'|"([^"\n]*)"|` + "`" + `([^` + "`" + `\n]*)` + "`" + `)`)

// badJSImports returns, for source at path file (relative to the repo root,
// slash-separated), every specifier that is neither a node: builtin nor a
// relative path that stays inside web/.
func badJSImports(file, src string) []string {
	var out []string
	for _, m := range jsSpecifier.FindAllStringSubmatch(src, -1) {
		spec := m[1] + m[2] + m[3]
		if strings.HasPrefix(spec, "node:") {
			continue
		}
		if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
			out = append(out, fmt.Sprintf("%s: import %q is not relative or node:", file, spec))
			continue
		}
		resolved := path.Join(path.Dir(file), spec)
		if !strings.HasPrefix(resolved, "web/") || strings.HasPrefix(resolved, "internal/") || strings.HasPrefix(resolved, "cmd/") {
			out = append(out, fmt.Sprintf("%s: import %q resolves to %s, outside web/", file, spec, resolved))
		}
	}
	return out
}

func TestWebClientImportsStayInWeb(t *testing.T) {
	t.Run("detector fires", func(t *testing.T) {
		src := "import { test } from 'node:test';\nimport { x } from '../../internal/x.js';\nimport './harness.mjs';\nconst m = await import(\"../../cmd/app/y.js\");\nimport y from 'left-pad';\n"
		got := badJSImports("web/jstest/a.test.mjs", src)
		if len(got) != 3 {
			t.Fatalf("want 3 violations (internal, cmd, bare), got %v", got)
		}
		for _, g := range got {
			if strings.Contains(g, "node:test") || strings.Contains(g, "harness.mjs") {
				t.Errorf("allowed specifier flagged: %s", g)
			}
		}
		if !strings.Contains(got[0], "../../internal/x.js") {
			t.Errorf("want ../../internal/x.js flagged first, got %v", got)
		}
	})

	root, _ := moduleRoot(t)
	var files []string
	for _, pattern := range []string{"web/static/js/*.js", "web/jstest/*.mjs"} {
		m, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			t.Fatal(err)
		}
		if len(m) == 0 {
			t.Fatalf("no files match %s; the scan is not seeing the web client", pattern)
		}
		files = append(files, m...)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range badJSImports(filepath.ToSlash(rel), string(data)) {
			t.Error(v)
		}
	}
}
