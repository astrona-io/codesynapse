package memory

import (
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
)

func TestContract(t *testing.T) {
	storetest.Run(t, New())
}
