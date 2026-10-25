package githistory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// Credentials are put together at run time so this file holds none.
var (
	removedToken = "gh" + "p_" + "Zx9Qw3Er7Ty1Ui5Op2As8Df4Gh6Jk0LmNbVcXz"
	keptToken    = "gh" + "p_" + "Pq8Wo2Ie6Ru4Ty0Al3Sk7Dj1Fh5Gz9XcVbNmQ"
	exampleKey   = "AKIA" + "IOSFODNN7EXAMPLE"
	pemLine      = "MIIEvQIBAD" + "ANBgkqhkiG" + "9w0BAQEFAA" + "SCBKcwggSj" + "AgEAAoIBAQ"
)

func TestScanSecrets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		_, err := gitx.Run(ctx, root, args...)
		require.NoError(t, err, strings.Join(args, " "))
	}
	commit := func(msg string, files map[string]string) {
		t.Helper()
		for name, body := range files {
			p := filepath.Join(root, name)
			if body == "" {
				require.NoError(t, os.Remove(p))
				continue
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
			require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
		}
		git("add", "-A")
		git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", msg)
	}
	git("init", "-q", "-b", "main")
	commit("add config", map[string]string{
		"config.go":      "package app\n\nconst token = \"" + removedToken + "\"\n",
		"docs.md":        "Use a key like " + exampleKey + ".\n",
		"deploy/key.pem": "-----BEGIN " + "RSA PRIVATE KEY-----\n" + pemLine + "\n-----END " + "RSA PRIVATE KEY-----\n",
	})
	commit("read the token from the environment", map[string]string{
		"config.go":      "package app\n\nimport \"os\"\n\nvar token = os.Getenv(\"TOKEN\")\n",
		"deploy/key.pem": "",
	})
	commit("add a client", map[string]string{"client.go": "package app\n\nconst other = \"" + keptToken + "\"\n"})

	g := model.NewGraph()
	require.NoError(t, Extract(ctx, "r", root, 10, g))
	require.NoError(t, ScanSecrets(ctx, "r", root, 10, g))

	got := map[string]model.Node{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding {
			got[n.File] = n
			for _, secret := range []string{removedToken, keptToken, pemLine} {
				assert.False(t, strings.Contains(n.Summary+n.ID+n.Name, secret), "a value is stored in "+n.ID)
			}
		}
	}
	assert.Len(t, got, 2, "the removed token and key; not the token still in the checkout, not the documentation example")
	tok := got["config.go"]
	assert.Equal(t, tok.Rule, historyRule)
	assert.Equal(t, tok.StartLine, 3)
	assert.Equal(t, tok.Language, "git-history")
	assert.Contains(t, tok.Summary, "GitHub token added in commit")
	assert.Contains(t, tok.Summary, "no longer in the checkout")
	key := got["deploy/key.pem"]
	assert.Equal(t, key.StartLine, 1)
	assert.Contains(t, key.Summary, "Private key")

	// The finding hangs off the commit that added it, the oldest one.
	owner := ""
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeHasFinding && e.To == tok.ID {
			owner = e.From
		}
	}
	n, ok := g.Node(owner)
	require.True(t, ok)
	assert.Equal(t, n.Kind, model.KindCommit)
	assert.Contains(t, n.Summary, "add config")
}

func TestHunkStartAndPaths(t *testing.T) {
	assert.Equal(t, hunkStart("@@ -0,0 +12,3 @@ func x()"), 12)
	assert.Equal(t, hunkStart("@@ -4 +7 @@"), 7)
	assert.Equal(t, patchPath("b/a/b.go"), "a/b.go")
	assert.Equal(t, patchPath("/dev/null"), "")
	assert.Equal(t, patchPath(`"b/sp ace.go"`), "sp ace.go")
}

func TestQuotedKeyHeaderIsNotAKey(t *testing.T) {
	patch := gitx.PatchRecord + "abc" + gitx.FieldSep + "2026-10-21T00:00:00Z\n+++ b/x_test.go\n@@ -0,0 +22,2 @@\n" +
		"+\t{in: \"-----BEGIN " + "RSA PRIVATE KEY-----\\nMIIE\\n-----END RSA PRIVATE KEY-----\"},\n+\t}\n"
	leaks, _ := parsePatches([]byte(patch), nil)
	assert.Len(t, leaks, 0)
}
