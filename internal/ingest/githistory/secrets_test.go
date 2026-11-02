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

// secretRepo is a git repository in a temporary directory and a function
// that commits files to it ("" removes one).
func secretRepo(t *testing.T) (string, func(msg string, files map[string]string)) {
	t.Helper()
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
		git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", msg)
	}
	git("init", "-q", "-b", "main")
	return root, commit
}

func TestScanSecrets(t *testing.T) {
	ctx := context.Background()
	root, commit := secretRepo(t)
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
	require.NoError(t, ScanSecrets(ctx, "r", root, 10, "", g))

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

// findings are the history findings ScanSecrets adds, by ID, with the
// commit each hangs off.
func findings(ctx context.Context, t *testing.T, root string, limit int, cacheFile string) map[string]string {
	t.Helper()
	g := model.NewGraph()
	require.NoError(t, Extract(ctx, "r", root, limit, g))
	require.NoError(t, ScanSecrets(ctx, "r", root, limit, cacheFile, g))
	got := map[string]string{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeHasFinding {
			n, ok := g.Node(e.To)
			require.True(t, ok)
			got[e.To] = e.From + " " + n.File + " " + n.Summary
		}
	}
	return got
}

func TestScanSecretsCache(t *testing.T) {
	ctx := context.Background()
	root, commit := secretRepo(t)
	cacheFile := filepath.Join(t.TempDir(), "cache", "secrets.json")
	token := func(i int) string {
		return "gh" + "p_" + strings.Repeat("Zx9Qw3Er7T", 3) + "abcdefghi"[i:i+1] + "Ui5Op2"
	}
	commit("one", map[string]string{"a.go": "package a\n\nconst t = \"" + token(0) + "\"\n"})
	commit("two", map[string]string{"a.go": "package a\n"})

	same := func(step string) {
		t.Helper()
		cached := findings(ctx, t, root, 10, cacheFile)
		assert.Equal(t, cached, findings(ctx, t, root, 10, ""), step)
		data, err := os.ReadFile(cacheFile)
		require.NoError(t, err)
		for i := range 5 {
			assert.False(t, strings.Contains(string(data), token(i)), step+": a value is in the cache")
		}
		c := loadSecretCache(cacheFile, 10)
		require.NotNil(t, c, step)
		assert.Equal(t, c.Head, gitx.HeadCommit(ctx, root), step)
	}
	same("first scan")
	assert.Len(t, findings(ctx, t, root, 10, cacheFile), 1)
	same("nothing new")

	commit("three", map[string]string{"b.go": "package a\n\nconst t = \"" + token(1) + "\"\n"})
	commit("four", map[string]string{"b.go": ""})
	same("new commits")
	assert.Len(t, findings(ctx, t, root, 10, cacheFile), 2)

	// A rewritten history is read again from the start.
	_, err := gitx.Run(ctx, root, "reset", "-q", "--hard", "HEAD~3")
	require.NoError(t, err)
	commit("five", map[string]string{"c.go": "package a\n\nconst t = \"" + token(2) + "\"\n"})
	commit("six", map[string]string{"c.go": ""})
	same("rewritten history")
	assert.Len(t, findings(ctx, t, root, 10, cacheFile), 1)

	// The limit counts matched commits, cached ones included.
	for i := range 3 {
		commit("more", map[string]string{"d.go": "package a\n\nconst t = \"" + token(3+i%2) + "\"\n"})
		commit("gone", map[string]string{"d.go": ""})
	}
	assert.Equal(t, findings(ctx, t, root, 3, cacheFile), findings(ctx, t, root, 3, ""))
	assert.Equal(t, findings(ctx, t, root, 3, cacheFile), findings(ctx, t, root, 3, ""))
	c := loadSecretCache(cacheFile, 3)
	require.NotNil(t, c)
	assert.Len(t, c.Commits, 3)
	assert.True(t, loadSecretCache(cacheFile, 10) == nil, "a cache for another limit is not used")
}

func TestLoadSecretCacheRefusesBadFiles(t *testing.T) {
	dir := t.TempDir()
	head := strings.Repeat("a", 40)
	good := secretCache{Head: head, Commits: []scannedCommit{{Hash: head, Hits: []hit{{Kind: "GitHub token", Key: strings.Repeat("b", 16), File: "a.go", Line: 3}}}}}
	path := filepath.Join(dir, "c.json")
	require.NoError(t, good.save(path, 5))
	require.NotNil(t, loadSecretCache(path, 5))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0o600))

	for name, edit := range map[string]func(c *secretCache){
		"parent path":   func(c *secretCache) { c.Commits[0].Hits[0].File = "../x" },
		"absolute path": func(c *secretCache) { c.Commits[0].Hits[0].File = "/etc/x" },
		"bad head":      func(c *secretCache) { c.Head = "HEAD" },
		"bad key":       func(c *secretCache) { c.Commits[0].Hits[0].Key = "value" },
		"too many":      func(c *secretCache) { c.Commits = append(c.Commits, make([]scannedCommit, 5)...) },
		"bad file key":  func(c *secretCache) { c.Files = map[string][]string{"x": nil} },
	} {
		t.Run(name, func(t *testing.T) {
			bad := good
			bad.Commits = []scannedCommit{good.Commits[0]}
			bad.Commits[0].Hits = []hit{good.Commits[0].Hits[0]}
			edit(&bad)
			p := filepath.Join(dir, name+".json")
			require.NoError(t, bad.save(p, 5))
			assert.True(t, loadSecretCache(p, 5) == nil)
		})
	}
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	assert.True(t, loadSecretCache(path, 5) == nil)
	assert.True(t, loadSecretCache(filepath.Join(dir, "missing.json"), 5) == nil)
}
