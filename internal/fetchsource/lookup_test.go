package fetchsource

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCached(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sources")
	u, err := Normalise("https://Docs.Example.com/guide?utm_source=x#intro")
	require.NoError(t, err)

	assert.False(t, Cached(dir, u), "missing folder")
	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "a lookup never creates the folder")

	c, err := OpenCache(dir, 0)
	require.NoError(t, err)
	assert.False(t, Cached(dir, u), "empty cache")
	require.NoError(t, c.Put(Key(u), Entry{URL: u.String(), Fetched: time.Now().Add(-48 * time.Hour), TTL: time.Hour, Text: "x"}))

	same, err := Normalise("https://docs.example.com:443/guide")
	require.NoError(t, err)
	assert.True(t, Cached(dir, same), "same normalised URL, stale entry included")
	other, err := Normalise("https://docs.example.com/other")
	require.NoError(t, err)
	assert.False(t, Cached(dir, other), "another page")
	assert.False(t, Cached("", u), "no folder")
	assert.False(t, Cached(dir, nil), "no URL")

	// An entry whose file holds another URL (a collision or a planted file)
	// does not count.
	require.NoError(t, c.Put(Key(other), Entry{URL: u.String()}))
	assert.False(t, Cached(dir, other), "entry for another URL")
}
