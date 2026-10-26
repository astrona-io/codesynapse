package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/auditarchive"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestAuditRetention(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	day := 24 * time.Hour
	setup := func(t *testing.T, arch auditarchive.Archive) (*Server, *accounts.Service) {
		acc := accounts.NewService(acctmem.New(), false)
		for _, age := range []time.Duration{500 * day, 200 * day, 100 * day, day} {
			_, err := acc.St.AddEvent(ctx, accounts.Event{At: now.Add(-age), Action: "user.login"})
			require.NoError(t, err)
		}
		return &Server{cfg: Config{Accounts: acc, AuditArchive: arch, AuditRetention: 400 * day, AuditHot: 90 * day},
			logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, acc
	}
	actions := func(t *testing.T, acc *accounts.Service) []string {
		evs, err := acc.St.ListEvents(ctx, accounts.EventFilter{Limit: 100, Ascending: true})
		require.NoError(t, err)
		var out []string
		for _, e := range evs {
			out = append(out, e.Action)
		}
		return out
	}

	t.Run("without an archive entries are pruned at the retention period", func(t *testing.T) {
		s, acc := setup(t, nil)
		s.applyAuditRetention(ctx, now)
		assert.Equal(t, actions(t, acc), []string{"user.login", "user.login", "user.login", "audit.prune"})
	})
	t.Run("with an archive old entries are sealed and expired files removed", func(t *testing.T) {
		arch, err := auditarchive.NewDir(t.TempDir())
		require.NoError(t, err)
		s, acc := setup(t, arch)
		s.applyAuditRetention(ctx, now)
		// One file holds every entry past the hot window; it expires only
		// once its newest entry is past the retention period.
		assert.Equal(t, actions(t, acc), []string{"user.login", "audit.seal"})
		segs, err := acc.St.AuditSegments(ctx)
		require.NoError(t, err)
		require.Len(t, segs, 1)
		assert.Equal(t, segs[0].Count, 3)

		s.applyAuditRetention(ctx, now.Add(400*day))
		assert.Equal(t, actions(t, acc), []string{"audit.seal", "audit.expire"})
		segs, err = acc.St.AuditSegments(ctx)
		require.NoError(t, err)
		require.Len(t, segs, 1)
		assert.Equal(t, segs[0].Count, 2)
		v, err := auditarchive.DeepVerify(ctx, acc.St, arch)
		require.NoError(t, err)
		assert.True(t, v.OK, v.Reason)
	})
	t.Run("sealed segments without an archive stop pruning", func(t *testing.T) {
		arch, err := auditarchive.NewDir(t.TempDir())
		require.NoError(t, err)
		s, acc := setup(t, arch)
		s.cfg.AuditRetention = 1000 * day
		s.applyAuditRetention(ctx, now)
		s.cfg.AuditArchive, s.cfg.AuditRetention = nil, 10*day
		before := actions(t, acc)
		s.applyAuditRetention(ctx, now)
		assert.Equal(t, actions(t, acc), before)
		v, err := accounts.VerifyAudit(ctx, acc.St)
		require.NoError(t, err)
		assert.True(t, v.OK, v.Reason)
	})
}
