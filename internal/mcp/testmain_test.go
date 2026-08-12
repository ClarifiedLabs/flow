// Package testmain routes internal/mcp tests through the hermetic test env.
package mcp

import (
	"testing"

	"github.com/ClarifiedLabs/flow/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }
