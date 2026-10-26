package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Log shipping to VictoriaLogs. Unset URL: services log to stderr only.
const (
	EnvLogsURL      = "CODESYNAPSE_LOGS_URL"
	EnvLogsUser     = "CODESYNAPSE_LOGS_USER"
	EnvLogsPassword = "CODESYNAPSE_LOGS_PASSWORD" //nolint:gosec // env var name, not a credential
)

// StartLogs sends env's log records to VictoriaLogs as well as stderr when
// CODESYNAPSE_LOGS_URL is set. It replaces env.Logger and returns ctx
// carrying it; call it first in a service's Run. Stop sends what is left.
func StartLogs(ctx context.Context, env *Env, service string, versions ...string) (context.Context, func(), error) {
	url := os.Getenv(EnvLogsURL)
	if url == "" {
		return ctx, func() {}, nil
	}
	if err := telemetry.CheckURL(url); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", EnvLogsURL, err)
	}
	version := "unknown"
	if len(versions) > 0 && versions[0] != "" {
		version = versions[0]
	}
	shipper := &telemetry.LogShipper{URL: url, User: os.Getenv(EnvLogsUser), Password: os.Getenv(EnvLogsPassword),
		StreamFields: []string{"service", "environment", "instance"}}
	// The shipped copy is always JSON, whatever --log-format says, and names
	// where it came from; the stderr copy stays as it was.
	shipped := slog.NewJSONHandler(shipper, &slog.HandlerOptions{Level: env.level}).WithAttrs([]slog.Attr{
		slog.String("service", "codesynapse-"+service), slog.String("environment", Getenv(EnvEnvironment, "unknown")),
		slog.String("instance", telemetry.InstanceID()), slog.String("version", version)})
	stderr := env.Logger
	inner := stderr.Handler()
	if th, ok := inner.(traceHandler); ok {
		inner = th.Handler // added once, around both copies
	}
	env.Logger = slog.New(traceHandler{slog.NewMultiHandler(inner, shipped)})

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Failures go to stderr only: through the shipper they would queue
		// behind the very store that is failing.
		shipper.Run(runCtx, func(err error) { stderr.Warn("shipping logs failed", "error", err) })
	}()
	env.Logger.Info("log shipping enabled", "service", service)
	return logx.WithLogger(ctx, env.Logger), sync.OnceFunc(func() {
		cancel()
		<-done
		final, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if err := shipper.Flush(final); err != nil {
			stderr.Warn("flushing logs failed", "error", err)
		}
	}), nil
}
