package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func replacementContract(t *testing.T, s store.Store) {
	ctx := context.Background()
	original, err := s.Node(ctx, ID(model.KindRepository, Repo))
	require.NoError(t, err)
	invalid := Fixture()
	root, _ := invalid.Node(original.ID)
	root.Head = "must-not-publish"
	invalid.AddEdge(model.Edge{From: original.ID, To: ID(model.KindFunction, "app.main"), Kind: model.EdgeKind("invalid")})
	require.Error(t, s.ReplaceRepo(ctx, Repo, invalid))
	got, err := s.Node(ctx, original.ID)
	require.NoError(t, err)
	assert.Equal(t, got.Head, original.Head)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, s.ReplaceRepo(canceled, Repo, Fixture()), context.Canceled)
	denied := errors.New("access revoked before publish")
	guarded := store.WithCommitCheck(ctx, func(context.Context) error { return denied })
	require.ErrorIs(t, s.ReplaceRepo(guarded, Repo, Fixture()), denied)
	got, err = s.Node(ctx, original.ID)
	require.NoError(t, err)
	assert.Equal(t, got.Head, original.Head)
}
