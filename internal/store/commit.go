package store

import "context"

type commitCheckKey struct{}

// WithCommitCheck attaches a live authorization and revision check to ingestion.
func WithCommitCheck(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, commitCheckKey{}, check)
}

// CommitCheck revalidates detached work immediately before publishing its graph.
func CommitCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check, ok := ctx.Value(commitCheckKey{}).(func(context.Context) error); ok && check != nil {
		return check(ctx)
	}
	return nil
}
