package gatewayproxy

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCleanModel(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-5":                    "claude-sonnet-4-5",
		"us.anthropic.claude-3-haiku-v1:0":     "us.anthropic.claude-3-haiku-v1:0",
		"claude-opus-4-1[1m]":                  otherModel, // the gateway's rule: no brackets
		"claude-3-haiku@2024":                  otherModel,
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
	start := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	for i := range MaxWindows + 5 {
		at := start.Add(time.Duration(i) * time.Hour)
		c.open(at)
		for m := range MaxModels + 3 {
			c.direct(at.Add(time.Minute), "model-"+strconv.Itoa(m))
		}
		c.close(at.Add(2 * time.Minute))
	}
	pending := c.pending(time.Now())
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
	assert.Len(t, again.pending(time.Now()), MaxWindows)

	again.drop(again.pending(time.Now())[:3])
	assert.Len(t, again.pending(time.Now()), MaxWindows-3)
}

func TestCountsLateAndOld(t *testing.T) {
	c := openCounts(filepath.Join(t.TempDir(), "counts.json"))
	now := time.Now()

	// A request that finished going direct after the outage closed gets a
	// closed window of its own; nothing is left open.
	c.open(now.Add(-time.Hour))
	c.direct(now.Add(-time.Hour), "m")
	c.close(now.Add(-time.Minute))
	c.direct(now, "m")
	assert.Equal(t, c.current() == nil, true, "no open window while the gateway is up")
	require.Len(t, c.pending(now), 2)

	// Windows the gateway would refuse as too old are dropped, not sent.
	c.open(now.Add(-maxAge - time.Hour))
	c.close(now.Add(-maxAge - time.Hour))
	assert.Len(t, c.pending(now), 2)
}

// A report reads its windows while requests keep being counted: pending
// hands out copies, so the race detector finds nothing here.
func TestCountsConcurrentReport(t *testing.T) {
	c := openCounts(filepath.Join(t.TempDir(), "counts.json"))
	at := time.Now().Add(-time.Hour)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			now := at.Add(time.Duration(i) * time.Second)
			c.open(now)
			c.direct(now, "model-"+strconv.Itoa(i%(MaxModels+2)))
			c.close(now)
		}
	})
	wg.Go(func() {
		for range 200 {
			for _, w := range c.pending(time.Now()) {
				for m, n := range w.Models {
					_, _ = m, n
				}
				if w.Models != nil {
					w.Models["mutated-by-the-reader"] = 1
				}
			}
		}
	})
	wg.Wait()
	for _, w := range c.pending(time.Now()) {
		_, ok := w.Models["mutated-by-the-reader"]
		assert.False(t, ok, "copies share nothing with the list")
	}
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
