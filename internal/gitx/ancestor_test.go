package gitx

import (
	"context"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestIsAncestorAndLogPatchesRange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := Run(ctx, dir, args...)
		require.NoError(t, err, strings.Join(args, " "))
		return strings.TrimSpace(string(out))
	}
	commit := func(msg string) string {
		t.Helper()
		git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", msg)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main")
	first := commit("first")
	second := commit("second")

	ok, err := IsAncestor(ctx, dir, first, second)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = IsAncestor(ctx, dir, second, first)
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = IsAncestor(ctx, dir, strings.Repeat("a", 40), second)
	assert.Error(t, err, "an unknown commit is an error, not false")
	_, err = IsAncestor(ctx, dir, "--all", second)
	assert.ErrorIs(t, err, ErrInvalidRev)

	_, err = LogPatchesRange(ctx, dir, 5, "x", "-x", second)
	assert.ErrorIs(t, err, ErrInvalidRev)
	_, err = LogPatchesRange(ctx, dir, 5, "x", first, "a..b")
	assert.ErrorIs(t, err, ErrInvalidRev)
}
