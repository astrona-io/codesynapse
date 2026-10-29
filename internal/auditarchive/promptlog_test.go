package auditarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestPromptLogRetention(t *testing.T) {
	ctx := context.Background()
	day := 24 * time.Hour
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setup := func(t *testing.T, days int) accounts.Store {
		st := memstore.New()
		require.NoError(t, st.CreateUser(ctx, accounts.User{ID: "u", Name: "u", Email: "u@example.com", CreatedAt: start}))
		require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "o", Name: "o", Slug: "o", CreatedAt: start}, "u"))
		require.NoError(t, st.SetPromptLogSetting(ctx, "o", accounts.PromptLogSetting{Enabled: true, RetentionDays: days}))
		// One row a day for ten days.
		for i := range 10 {
			_, err := st.AddPromptLog(ctx, accounts.PromptLogEntry{OrgID: "o", At: start.Add(time.Duration(i) * day), Route: "cloud", Prompt: "p"})
			require.NoError(t, err)
		}
		return st
	}
	rows := func(t *testing.T, st accounts.Store) int {
		got, err := st.ListPromptLog(ctx, accounts.PromptLogFilter{OrgID: "o"})
		require.NoError(t, err)
		return len(got)
	}
	now := start.Add(10 * day)

	t.Run("without an archive rows past retention are deleted", func(t *testing.T) {
		st := setup(t, 4)
		res, err := PromptLogRetention(ctx, st, nil, 2*day, now)
		require.NoError(t, err)
		assert.Equal(t, res, PromptLogResult{Pruned: 6})
		assert.Equal(t, rows(t, st), 4)
	})

	t.Run("with an archive rows past the hot window are sealed, then expire", func(t *testing.T) {
		st := setup(t, 6)
		arch, err := NewDir(t.TempDir())
		require.NoError(t, err)
		res, err := PromptLogRetention(ctx, st, arch, 2*day, now)
		require.NoError(t, err)
		// Days 0-3 are past six days of retention; days 4-7 past two hot days.
		assert.Equal(t, res, PromptLogResult{Pruned: 4, Sealed: 1})
		assert.Equal(t, rows(t, st), 2)
		segs, err := st.PromptLogSegments(ctx, now)
		require.NoError(t, err)
		require.Len(t, segs, 1)
		assert.Equal(t, segs[0].Count, 4)
		body, err := arch.Get(ctx, segs[0].Key)
		require.NoError(t, err)
		sum := sha256.Sum256(body)
		assert.Equal(t, hex.EncodeToString(sum[:]), segs[0].SHA256, "the recorded digest matches the file")
		kept, err := DecodePromptLog(body)
		require.NoError(t, err)
		require.Len(t, kept, 4)
		assert.Equal(t, kept[0].Prompt, "p")

		// Once the newest sealed row (day 7) is six days old, the file goes.
		res, err = PromptLogRetention(ctx, st, arch, 2*day, start.Add(14*day))
		require.NoError(t, err)
		assert.Equal(t, res, PromptLogResult{Sealed: 1, Expired: 1}, "days 8-9 are sealed in turn")
		_, err = arch.Get(ctx, segs[0].Key)
		assert.True(t, errors.Is(err, fs.ErrNotExist))
		left, err := st.PromptLogSegments(ctx, start.Add(30*day))
		require.NoError(t, err)
		require.Len(t, left, 1)
		assert.True(t, left[0].ID != segs[0].ID)
		assert.Equal(t, rows(t, st), 0)
	})

	t.Run("a hot window beyond retention only prunes", func(t *testing.T) {
		st := setup(t, 3)
		arch, err := NewDir(t.TempDir())
		require.NoError(t, err)
		res, err := PromptLogRetention(ctx, st, arch, 90*day, now)
		require.NoError(t, err)
		assert.Equal(t, res, PromptLogResult{Pruned: 7})
	})
}

func TestPromptLogKeys(t *testing.T) {
	assert.NoError(t, checkKey(promptLogKey(1, 2)))
	assert.Error(t, checkKey("promptlog-../x.jsonl.gz"))
}
