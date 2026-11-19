package archtest

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codeownerLine represents a parsed entry in CODEOWNERS.
type codeownerLine struct {
	RawNum  int
	Pattern string
	Owners  []string
}

func parseCodeowners(content string) ([]codeownerLine, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNum := 0
	var entries []codeownerLine

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pattern := fields[0]
		owners := fields[1:]

		if len(owners) == 0 {
			return nil, fmt.Errorf("line %d: pattern %q has no owner", lineNum, pattern)
		}

		for _, owner := range owners {
			if !strings.HasPrefix(owner, "@") {
				return nil, fmt.Errorf("line %d: owner %q must begin with @", lineNum, owner)
			}
			if strings.HasPrefix(owner, "@app/") || strings.Contains(owner, "[bot]") || owner == "@app/quarterback-ai-dev-environment" || strings.Contains(owner, "quarterback-ai-dev-environment") {
				return nil, fmt.Errorf("line %d: bot/app owner %q is forbidden", lineNum, owner)
			}
		}

		entries = append(entries, codeownerLine{
			RawNum:  lineNum,
			Pattern: pattern,
			Owners:  owners,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return entries, nil
}

func validateCodeownersRules(entries []codeownerLine, repoRoot string) error {
	hasCatchAll := false
	explicitRequired := map[string]bool{
		"/db/migrations/":     false,
		"/.github/":           false,
		"/.github/CODEOWNERS": false,
	}

	for _, entry := range entries {
		if entry.Pattern == "*" {
			hasCatchAll = true
		}
		if _, ok := explicitRequired[entry.Pattern]; ok {
			explicitRequired[entry.Pattern] = true
		}

		if entry.Pattern != "*" {
			matched, err := patternMatchesTree(entry.Pattern, repoRoot)
			if err != nil {
				return fmt.Errorf("line %d: pattern %q check error: %w", entry.RawNum, entry.Pattern, err)
			}
			if !matched {
				return fmt.Errorf("line %d: pattern %q matches nothing in the tree", entry.RawNum, entry.Pattern)
			}
		}
	}

	if !hasCatchAll {
		return errors.New("missing '*' catch-all rule")
	}

	for req, seen := range explicitRequired {
		if !seen {
			return fmt.Errorf("missing required explicit rule for %q", req)
		}
	}

	return nil
}

func patternMatchesTree(pattern, repoRoot string) (bool, error) {
	cleanPat := strings.TrimPrefix(pattern, "/")
	cleanPat = strings.TrimSuffix(cleanPat, "/")

	targetPath := filepath.Join(repoRoot, filepath.FromSlash(cleanPat))
	if _, err := os.Stat(targetPath); err == nil {
		return true, nil
	}

	matchedAny := false
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == repoRoot {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return err
		}
		slashRel := filepath.ToSlash(rel)
		if slashRel == cleanPat || strings.HasPrefix(slashRel, cleanPat+"/") {
			matchedAny = true
			return fs.SkipDir
		}
		return nil
	})

	if matchedAny {
		return true, nil
	}
	return false, err
}

func TestCodeowners(t *testing.T) {
	repoRoot, _ := moduleRoot(t)
	codeownersPath := filepath.Join(repoRoot, ".github", "CODEOWNERS")

	data, err := os.ReadFile(codeownersPath)
	if err != nil {
		t.Fatalf("reading .github/CODEOWNERS: %v", err)
	}

	entries, err := parseCodeowners(string(data))
	if err != nil {
		t.Fatalf("parse .github/CODEOWNERS: %v", err)
	}

	if err := validateCodeownersRules(entries, repoRoot); err != nil {
		t.Fatalf("validating .github/CODEOWNERS: %v", err)
	}

	for _, entry := range entries {
		for _, owner := range entry.Owners {
			if owner != "@trriplejay" {
				t.Errorf("line %d: expected owner @trriplejay, got %q", entry.RawNum, owner)
			}
		}
	}

	t.Run("RejectBotsAndMalformed", func(t *testing.T) {
		tests := []struct {
			name        string
			content     string
			wantErrSub  string
			checkTree   bool
		}{
			{
				name:       "bot app prefix",
				content:    "* @app/mybot\n",
				wantErrSub: "bot/app owner",
			},
			{
				name:       "bot quarterback dev env",
				content:    "* @app/quarterback-ai-dev-environment\n",
				wantErrSub: "bot/app owner",
			},
			{
				name:       "bot bracket",
				content:    "* @dependabot[bot]\n",
				wantErrSub: "bot/app owner",
			},
			{
				name:       "missing owner",
				content:    "*\n",
				wantErrSub: "has no owner",
			},
			{
				name:       "owner without at sign",
				content:    "* trriplejay\n",
				wantErrSub: "must begin with @",
			},
			{
				name: "missing catch-all",
				content: "/db/migrations/ @trriplejay\n/.github/ @trriplejay\n/.github/CODEOWNERS @trriplejay\n",
				wantErrSub: "missing '*' catch-all",
				checkTree:  true,
			},
			{
				name: "missing explicit rule",
				content: "* @trriplejay\n/db/migrations/ @trriplejay\n/.github/ @trriplejay\n",
				wantErrSub: "missing required explicit rule for \"/.github/CODEOWNERS\"",
				checkTree:  true,
			},
			{
				name: "non-wildcard pattern matches nothing",
				content: "* @trriplejay\n/db/migrations/ @trriplejay\n/.github/ @trriplejay\n/.github/CODEOWNERS @trriplejay\n/nonexistent/path/ @trriplejay\n",
				wantErrSub: "matches nothing in the tree",
				checkTree:  true,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				entries, err := parseCodeowners(tc.content)
				if err == nil && tc.checkTree {
					err = validateCodeownersRules(entries, repoRoot)
				}
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErrSub, err)
				}
			})
		}
	})
}
