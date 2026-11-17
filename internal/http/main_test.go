package http_test

import (
	"os"
	"testing"

	"github.com/softsrv/chorus/internal/testnet"
)

func TestMain(m *testing.M) {
	restore := testnet.RefuseExternal()
	code := m.Run()
	restore()
	os.Exit(code)
}
