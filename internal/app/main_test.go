package app

import (
	"os"
	"testing"

	"github.com/softsrv/chorus/internal/testnet"
)

func TestMain(m *testing.M) {
	testnet.RefuseExternal()
	os.Exit(m.Run())
}
