package runledger

import (
	"os"
	"testing"

	"github.com/joellarson/togi/features/internal/harness"
)

func TestMain(m *testing.M) {
	os.Exit(harness.Main(m))
}
