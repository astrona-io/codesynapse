// Package logx carries a *slog.Logger through a context.
package logx

import (
	"context"
	"log/slog"
)

type ctxKey struct{}

// WithLogger returns ctx carrying l.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger in ctx, or slog.Default().
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// Error returns an "error" attribute for err.
func Error(err error) slog.Attr { return slog.Any("error", err) }
