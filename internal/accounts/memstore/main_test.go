package memstore

import (
	"os"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// TestMain uses cheap argon2id parameters: the contract creates many
// accounts, and production costs take minutes under the race detector.
// Hashes verify the same way, so no assertion changes.
func TestMain(m *testing.M) {
	accounts.UseFastPasswordHashing()
	os.Exit(m.Run())
}
