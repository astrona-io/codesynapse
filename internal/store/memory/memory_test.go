package memory

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/store/storetest"
)

func TestContract(t *testing.T) {
	storetest.Run(t, New())
}
