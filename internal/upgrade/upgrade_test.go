package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
}

const libV1 = `package lib

type Client struct {
	Name    string
	Timeout int
	secret  string
}

func New(name string) *Client { return &Client{Name: name} }

func (c *Client) Do(path string, n int) error { return nil }

func (c *Client) Close() error { return nil }

type Level int

const (
	Debug Level = iota
	Info
)

type Store interface {
	Get(key string) (string, error)
	seal()
}

func Map[T any](xs []T, f func(T) T) []T { return xs }

var Default = New("default")
`

const libV2 = `package lib

type Client struct {
	Name    string
	Timeout float64
}

func New(label string) *Client { return &Client{Name: label} }

func (c *Client) Do(ctx any, path string, n int) error { return nil }

func (c *Client) Close() error { return nil }

func (c *Client) Ping() error { return nil }

type Level string

const (
	Debug Level = "debug"
	Info  Level = "info"
)

type Store interface {
	Get(k string) (string, error)
	seal()
}

func Map[T any](xs []T, f func(T) T) []T { return xs }
`

func TestAPIAndDiff(t *testing.T) {
	dir := t.TempDir()
	v1, v2 := filepath.Join(dir, "v1"), filepath.Join(dir, "v2")
	write(t, v1, map[string]string{
		"lib.go":              libV1,
		"lib_test.go":         "package lib\n\nfunc TestHelper() {}\n",
		"internal/x/x.go":     "package x\n\nfunc Hidden() {}\n",
		"sub/sub.go":          "package sub\n\nfunc Gone() {}\n",
		"nested/go.mod":       "module example.com/lib/nested\n",
		"nested/n.go":         "package nested\n\nfunc N() {}\n",
		"cmd/tool/main.go":    "package main\n\nfunc Run() {}\n",
		"testdata/fixture.go": "package fixture\n\nfunc F() {}\n",
	})
	write(t, v2, map[string]string{"lib.go": libV2})

	old, err := API(v1, "example.com/lib")
	require.NoError(t, err)
	for _, key := range []string{"example.com/lib.TestHelper", "example.com/lib/internal/x.Hidden", "example.com/lib/nested.N", "example.com/lib/cmd/tool.Run", "example.com/lib.Client.secret"} {
		_, ok := old[key]
		assert.False(t, ok, key)
	}
	assert.Equal(t, old["example.com/lib.Info"], "const Level", "an iota constant repeats the type before it")
	assert.Equal(t, old["example.com/lib.Map"], "func[T any]([]T, func(T) T) []T")
	assert.Equal(t, old["example.com/lib.Client.Do"], "(*) func(string, int) error")
	assert.Equal(t, old["example.com/lib.Store"], "type interface{seal()}")

	newAPI, err := API(v2, "example.com/lib")
	require.NoError(t, err)
	got := map[string]string{}
	for _, c := range Diff(old, newAPI) {
		got[c.Symbol] = c.Kind
	}
	assert.Equal(t, got, map[string]string{
		"example.com/lib.Client.Do":      "changed",
		"example.com/lib.Client.Timeout": "changed",
		"example.com/lib.Client.Ping":    "added",
		"example.com/lib.Level":          "changed",
		"example.com/lib.Default":        "removed",
		"example.com/lib/sub.Gone":       "removed",
	}, "renamed parameters (New, Store.Get) and new constant values (Debug) are not changes")
}

// TestAnalyze runs the whole command on a vendored repository, so go list
// needs no network, with the two versions in a module cache of its own.
func TestAnalyze(t *testing.T) {
	dir := t.TempDir()
	repo, cache := filepath.Join(dir, "repo"), filepath.Join(dir, "cache")
	write(t, repo, map[string]string{
		"go.mod":                        "module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v1.0.0\n",
		"vendor/modules.txt":            "# example.com/lib v1.0.0\n## explicit; go 1.22\nexample.com/lib\n",
		"vendor/example.com/lib/lib.go": libV1,
		"main.go": `package main

import "example.com/lib"

func main() {
	c := lib.New("x")
	_ = c.Do("/", 1)
	_ = c.Close()
	_ = lib.Client{Timeout: 3}
	_ = c.Name
	_ = lib.Info
	_ = lib.Default
}
`,
	})
	write(t, filepath.Join(cache, "example.com", "lib@v1.0.0"), map[string]string{"lib.go": libV1})
	write(t, filepath.Join(cache, "example.com", "lib@v1.1.0"), map[string]string{"lib.go": libV2})
	t.Setenv("GOMODCACHE", cache)
	t.Setenv("GOFLAGS", "")
	ctx := context.Background()

	r, err := Analyze(ctx, Options{Root: repo, Module: "example.com/lib", To: "v1.1.0"})
	require.NoError(t, err)
	assert.Equal(t, r.From, "v1.0.0")
	assert.True(t, r.Compared)
	syms := map[string]bool{}
	for _, u := range r.Uses {
		assert.Equal(t, u.File, "main.go")
		assert.Equal(t, u.In, "main")
		syms[u.Symbol] = true
	}
	for _, s := range []string{"example.com/lib.New", "example.com/lib.Client", "example.com/lib.Client.Do", "example.com/lib.Client.Close",
		"example.com/lib.Client.Timeout", "example.com/lib.Client.Name", "example.com/lib.Info", "example.com/lib.Default"} {
		assert.True(t, syms[s], s)
	}
	var breaking []string
	for _, c := range r.Breaking {
		breaking = append(breaking, c.Kind+" "+c.Symbol)
	}
	assert.Equal(t, breaking, []string{"changed example.com/lib.Client.Do", "changed example.com/lib.Client.Timeout", "removed example.com/lib.Default"},
		"Info's own declaration did not change, and Level is not named in main.go")
	assert.Equal(t, r.Added, 1)

	var out strings.Builder
	require.NoError(t, WriteText(&out, r))
	assert.Contains(t, out.String(), "3 declarations this repository uses changed")
	assert.Contains(t, out.String(), "now (*) func(any, string, int) error")
	assert.Contains(t, out.String(), "main.go:7 (main)")

	r, err = Analyze(ctx, Options{Root: repo, Module: "example.com/lib", To: "v1.2.0"})
	require.NoError(t, err)
	assert.False(t, r.Compared)
	assert.Equal(t, r.Missing, []string{"v1.2.0"})

	_, err = Analyze(ctx, Options{Root: repo, Module: "example.com/other"})
	assert.Error(t, err)
	_, err = Analyze(ctx, Options{Root: repo, Module: "example.com/lib", To: "latest"})
	assert.Error(t, err, "only exact versions: nothing resolves a query")
}
