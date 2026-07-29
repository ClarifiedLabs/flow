package terminalbridge

import (
	"os"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Main(m)
}

var _ = os.Exit
