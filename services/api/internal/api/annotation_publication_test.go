package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/annotation"
)

func annotationFixtureResult(t *testing.T, f runtimeFixture) annotation.Result {
	t.Helper()
	p := indexedPolicy(t, f, annotationConfig)
	approvePolicy(t, f, p)
	repo, err := f.store.Node(t.Context(), storetest.ID(model.KindRepository, storetest.Repo))
	require.NoError(t, err)
	symbol, err := f.store.Node(t.Context(), storetest.ID(model.KindFunction, "app.main"))
	require.NoError(t, err)
	return annotation.Result{
		Job:          annotation.Job{ID: "annotation-job", Execution: f.identity, Graph: storetest.Repo, Commit: repo.Head, Revision: repo.Time, PolicyDigest: p.Digest()},
		Items:        []annotation.Item{{SymbolID: symbol.ID, Summary: "Main entry point"}},
		Fingerprints: map[string]string{symbol.ID: annotationFingerprint(symbol)},
	}
}

func TestAnnotationPersistenceRejectsConcurrentPublication(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	result := annotationFixtureResult(t, f)
	ctx := runtimeContext(t, f)
	locked, release := make(chan struct{}), make(chan struct{})
	published := make(chan error, 1)
	go func() {
		published <- f.server.publishRepo(ctx, storetest.Repo, func(txCtx context.Context) error {
			close(locked)
			<-release
			g := storetest.Fixture()
			old, err := f.store.Node(txCtx, storetest.ID(model.KindRepository, storetest.Repo))
			if err != nil {
				return err
			}
			root, _ := g.Node(old.ID)
			root.Meta = old.Meta
			root.Time = "next-revision"
			return f.store.ReplaceRepo(txCtx, storetest.Repo, g)
		})
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("publication lock was not acquired")
	}
	saved := make(chan error, 1)
	go func() { saved <- f.server.persistAnnotations(ctx, result) }()
	select {
	case err := <-saved:
		t.Fatalf("annotation bypassed publication lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-published:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not complete")
	}
	select {
	case err := <-saved:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("annotation persistence deadlocked")
	}
	rows, err := f.accounts.St.ListAnnotations(ctx, f.org.ID, storetest.Repo, 100)
	require.NoError(t, err)
	assert.Len(t, rows, 0)
}

func TestAnnotationPersistenceRechecksOriginalCredential(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	result := annotationFixtureResult(t, f)
	denied := errors.New("original API key was revoked")
	ctx := store.WithCommitCheck(runtimeContext(t, f), func(context.Context) error { return denied })
	assert.ErrorIs(t, f.server.persistAnnotations(ctx, result), denied)
	rows, err := f.accounts.St.ListAnnotations(ctx, f.org.ID, storetest.Repo, accounts.MaxRepositoryAnnotations)
	require.NoError(t, err)
	assert.Len(t, rows, 0)
}

func TestAnnotationPersistenceCommitsAuthorizedBatch(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	result := annotationFixtureResult(t, f)
	ctx := store.WithCommitCheck(runtimeContext(t, f), func(context.Context) error { return nil })
	require.NoError(t, f.server.persistAnnotations(ctx, result))
	rows, err := f.accounts.St.ListAnnotations(ctx, f.org.ID, storetest.Repo, 100)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}
