//go:build systest

// Package systest tests the running system: it starts the real app (the
// binary TestMain builds, or the Docker image named by SYSTEST_IMAGE) beside
// a real Postgres (TEST_DATABASE_URL, already migrated) and an in-test SMTP
// capture server, and drives it over HTTP the way a client does. It imports
// no chorus package; it only execs what cmd/app builds to.
package systest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// appBinary is the path of the app TestMain built (binary mode only).
var appBinary string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	// No database means no system under test. Fail rather than skip, so a
	// run (or a CI job) that lost the variable goes red instead of passing
	// with nothing exercised.
	if os.Getenv("TEST_DATABASE_URL") == "" {
		fmt.Fprintln(os.Stderr, "systest: TEST_DATABASE_URL is not set; the system tests need a migrated database: set it in .env (see .env.example) or the environment, then run make test-system, which migrates it first")
		return 1
	}
	if os.Getenv("SYSTEST_IMAGE") != "" {
		// Image mode: the image is the system under test; nothing to build.
		return m.Run()
	}

	dir, err := os.MkdirTemp("", "chorus-systest-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "systest: create build dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)

	appBinary = filepath.Join(dir, "app")
	build := exec.Command("go", "build", "-o", appBinary, "github.com/softsrv/chorus/cmd/app")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "systest: go build ./cmd/app: %v\n", err)
		return 1
	}
	return m.Run()
}
