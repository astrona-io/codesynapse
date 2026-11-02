package ingest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestSecretCacheFileOutsideTheCheckout(t *testing.T) {
	root := t.TempDir()
	assert.Equal(t, secretCacheFile("", root, "r"), "")
	assert.Equal(t, secretCacheFile(root, root, "r"), "", "the checkout itself")
	assert.Equal(t, secretCacheFile(filepath.Join(root, ".cache"), root, "r"), "", "inside the checkout")
	cache := t.TempDir()
	a, b := secretCacheFile(cache, root, "o/a"), secretCacheFile(cache, root, "o/b")
	assert.True(t, strings.HasPrefix(a, cache+string(filepath.Separator)))
	assert.NotEqual(t, a, b)
}
