package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
)

// Metrics settings. The password is read from the environment only.
const (
	EnvMetricsURL      = "CODESYNAPSE_METRICS_URL"
	EnvMetricsUser     = "CODESYNAPSE_METRICS_USER"
	EnvMetricsPassword = "CODESYNAPSE_METRICS_PASSWORD" //nolint:gosec // env var name, not a credential
)

// StartMetrics pushes telemetry.Default to VictoriaMetrics every 15s when
// CODESYNAPSE_METRICS_URL is set. The returned stop pushes a last time and
// waits for it.
func StartMetrics(ctx context.Context, logger *slog.Logger, job string) (stop func(), err error) {
	url := os.Getenv(EnvMetricsURL)
	if url == "" {
		return func() {}, nil
	}
	if err := telemetry.CheckURL(url); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvMetricsURL, err)
	}
	host, _ := os.Hostname() //nolint:errcheck // an empty instance label is acceptable
	p := &telemetry.Pusher{URL: url, User: Getenv(EnvMetricsUser, ""), Password: os.Getenv(EnvMetricsPassword), Job: job, Instance: host}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx, 15*time.Second, func(err error) { logger.Warn("pushing metrics failed", "error", err) })
	}()
	logger.Info("metrics push enabled", "job", job, "every", "15s")
	return func() { cancel(); <-done }, nil
}
