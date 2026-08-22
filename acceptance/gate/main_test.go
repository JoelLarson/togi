package gate

import (
	"os"
	"testing"

	"github.com/joellarson/togi/acceptance/internal/harness"
)

func TestMain(m *testing.M) {
	os.Exit(harness.Main(m))
}
