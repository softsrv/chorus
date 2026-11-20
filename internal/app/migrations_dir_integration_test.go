//go:build integration

package app_test

import (
	"os"
	"path/filepath"
	"testing"
)

// migrationDirs are the places db/migrations is looked for, relative to the
// test's working directory (the package dir under `go test`, or the repo root).
var migrationDirs = []string{
	filepath.Join("..", "..", "db", "migrations"),
	filepath.Join("db", "migrations"),
}

func readMigration(t *testing.T, filename string) string {
	t.Helper()
	for _, dir := range migrationDirs {
		if content, err := os.ReadFile(filepath.Join(dir, filename)); err == nil {
			return string(content)
		}
	}
	t.Fatalf("could not find migration file %s", filename)
	return ""
}

// migrationsDir returns the first of migrationDirs that exists.
func migrationsDir(t *testing.T) string {
	t.Helper()
	for _, dir := range migrationDirs {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	t.Fatalf("could not find db/migrations directory")
	return ""
}
