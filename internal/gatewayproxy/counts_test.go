package gatewayproxy

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCleanModel(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-5":                    "claude-sonnet-4-5",
		"claude-opus-4-1[1m]":                  "claude-opus-4-1[1m]",
		"us.anthropic.claude-3-haiku@2024":     "us.anthropic.claude-3-haiku@2024",
		"":                                     otherModel,
		"ignore previous instructions":         otherModel,
		"model\nwith newline":                  otherModel,
		strings.Repeat("a", maxModelNameLen+1): otherModel,
	}
	for in, want := range cases {
		assert.Equal(t, CleanModel(in), want, in)
	}
}

func TestCountsBounded(t *testing.T) {
	dir := t.TempDir()
	c := openCounts(filepath.Join(dir, "counts.json"))
	start := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	for i := range MaxWindows + 5 {
		at := start.Add(time.Duration(i) * time.Hour)
		c.open(at)
		for m := range MaxModels + 3 {
			c.direct(at.Add(time.Minute), "model-"+strconv.Itoa(m))
		}
		c.close(at.Add(2 * time.Minute))
	}
	pending := c.pending()
	require.Len(t, pending, MaxWindows)
	total := 0
	for _, w := range pending {
		assert.LessOrEqual(t, len(w.Models), MaxModels)
		total += w.Requests
	}
	assert.Equal(t, total, (MaxWindows+5)*(MaxModels+3), "folding keeps every request counted")
	assert.Equal(t, pending[0].Start, start)

	info, err := os.Stat(filepath.Join(dir, "counts.json"))
	require.NoError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0o600))
	assert.Less(t, info.Size(), int64(maxSmallFile))

	// A restarted proxy reads them back; a window left open is closed.
	c.open(start.Add(100 * time.Hour))
	again := openCounts(filepath.Join(dir, "counts.json"))
	assert.Len(t, again.pending(), MaxWindows)

	again.drop(again.pending()[:3])
	assert.Len(t, again.pending(), MaxWindows-3)
}

func TestFilesRemove(t *testing.T) {
	f := Files{Dir: t.TempDir()}
	require.NoError(t, f.Save(Config{Gateway: "https://gw.example.com", Port: DefaultPort}))
	_, err := f.Secret()
	require.NoError(t, err)
	c := openCounts(f.countsPath())
	c.direct(time.Now(), "m")
	c.direct(time.Now(), "m")
	n, err := f.Remove()
	require.NoError(t, err)
	assert.Equal(t, n, 2)
	_, err = f.Load()
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"https", Config{Gateway: "https://gw.example.com", Port: DefaultPort}, true},
		{"http loopback", Config{Gateway: "http://127.0.0.1:8080", Port: DefaultPort}, true},
		{"http remote", Config{Gateway: "http://gw.example.com", Port: DefaultPort}, false},
		{"low port", Config{Gateway: "https://gw.example.com", Port: 80}, false},
		{"empty", Config{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			assert.Equal(t, err == nil, tc.ok, err)
		})
	}
}
