//go:build systest

package systest

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	dbAdminTimeout = 30 * time.Second
	// The first `go run` of the pinned migrate CLI may download and build it.
	migrateTimeout = 5 * time.Minute
)

// freshDatabase creates a uniquely named database on the server
// TEST_DATABASE_URL points at, migrates it with the same pinned migrate CLI
// CI and the Makefile use, and drops it when the test ends. It returns the
// new database's URL. Nothing else ever writes to it, so a test started on it
// runs against a freshly migrated, empty database whatever ran before on the
// shared TEST_DATABASE_URL database (the integration suite leaves data there).
func freshDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	name := "chorus_systest_" + randHex(t, 6)
	ident := pgx.Identifier{name}.Sanitize()

	admin := func(what, sql string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), dbAdminTimeout)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Fatalf("%s: connect to TEST_DATABASE_URL: %v", what, err)
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v (the TEST_DATABASE_URL role needs CREATEDB)", what, err)
		}
	}

	admin("create database "+name, "CREATE DATABASE "+ident)
	t.Cleanup(func() { admin("drop database "+name, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)") })

	u.Path = "/" + name
	fresh := u.String()

	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()
	migrate := exec.CommandContext(ctx, "go", "run", "-tags", "postgres",
		"github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1",
		"-path", "../db/migrations", "-database", fresh, "up")
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("migrate %s up: %v\n%s", name, err, out)
	}
	return fresh
}

// countUsers returns the number of rows in the users table of the database
// at dbURL.
func countUsers(t *testing.T, dbURL string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dbAdminTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("count users: connect: %v", err)
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}
