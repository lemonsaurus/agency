package tmux

import (
	"os"
	"testing"

	"github.com/lemonsaurus/agency/internal/tmuxtest"
)

func TestMain(m *testing.M) {
	tmuxtest.Sweep()
	os.Exit(m.Run())
}
