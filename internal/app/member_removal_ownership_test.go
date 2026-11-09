package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMemberRemovalLeavesQuestsToQuestCode: only the quest part of the app
// changes quests when a member is removed. The family-member code and its
// queries never name the quest table or its holder columns, the release
// query lives in quests.sql, and the one hand-written Go code that runs it is
// releaseMemberQuests in quest_service.go.
func TestMemberRemovalLeavesQuestsToQuestCode(t *testing.T) {
	const repoRoot = "../.."
	forbidden := regexp.MustCompile(`(?i)household_quests|claimed_by|claimed_at|done_at`)
	for _, path := range []string{
		"family_service.go",
		filepath.Join(repoRoot, "db", "queries", "family_members.sql"),
		filepath.Join(repoRoot, "internal", "db", "family_members.sql.go"),
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if m := forbidden.FindString(line); m != "" {
				t.Errorf("%s:%d names %q; quests belong to the quest code: %s", path, i+1, m, strings.TrimSpace(line))
			}
		}
	}

	// The release query is defined once, in quests.sql.
	queries, err := filepath.Glob(filepath.Join(repoRoot, "db", "queries", "*.sql"))
	if err != nil || len(queries) == 0 {
		t.Fatalf("list queries: %v (%d files)", err, len(queries))
	}
	defined := regexp.MustCompile(`(?m)^-- name: ReleaseMemberQuests\b`)
	var definedIn []string
	for _, path := range queries {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for range defined.FindAll(src, -1) {
			definedIn = append(definedIn, filepath.Base(path))
		}
	}
	if len(definedIn) != 1 || definedIn[0] != "quests.sql" {
		t.Errorf("ReleaseMemberQuests defined in %v, want only quests.sql", definedIn)
	}

	// Its only hand-written caller is releaseMemberQuests in quest_service.go.
	generated := filepath.Join(repoRoot, "internal", "db")
	var callers []string
	err = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web":
				return filepath.SkipDir
			}
			if path == generated {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ReleaseMemberQuests" {
					callers = append(callers, filepath.ToSlash(filepath.Clean(path))+"#"+fn.Name.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	want := filepath.ToSlash(filepath.Join(repoRoot, "internal", "app", "quest_service.go")) + "#releaseMemberQuests"
	if len(callers) != 1 || callers[0] != want {
		t.Errorf("ReleaseMemberQuests called from %v, want only %s", callers, want)
	}
}
