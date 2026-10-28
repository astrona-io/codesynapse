package golang

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestHardcodedSecretSkipsCounterLabels: a constant named Token… whose value
// is a parenthesised counter label ("(tokens:input)") is not a credential.
// A real brief listed internal/usage/tokens.go as a high finding (PLAN item
// 18); credential-looking values beside it are still reported.
func TestHardcodedSecretSkipsCounterLabels(t *testing.T) {
	root, err := filepath.Abs("testdata/secrets")
	require.NoError(t, err)
	res, err := Extract(context.Background(), Options{Repo: "secrets", Root: root})
	require.NoError(t, err)
	var flagged []string
	for _, n := range res.Graph.Nodes() {
		if n.Kind == model.KindFinding && n.Rule == "hardcoded-secret" {
			name, _, _ := strings.Cut(n.Summary, " ")
			flagged = append(flagged, name)
		}
	}
	for _, label := range []string{"TokenInput", "TokenCached", "TokenTurns"} {
		assert.NotContains(t, flagged, label)
	}
	for _, secret := range []string{"apiToken", "sessionToken", "adminPassword"} {
		assert.Contains(t, flagged, secret)
	}
}
