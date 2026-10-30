package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Metrics settings. The password is read from the environment only.
const (
	EnvMetricsURL      = "CODESYNAPSE_METRICS_URL"
	EnvMetricsUser     = "CODESYNAPSE_METRICS_USER"
	EnvMetricsPassword = "CODESYNAPSE_METRICS_PASSWORD" //nolint:gosec // env var name, not a credential
	EnvMetricsExporter = "CODESYNAPSE_METRICS_EXPORTER"
	EnvEnvironment     = "CODESYNAPSE_ENVIRONMENT"
	// EnvAlertsURL is the internal vmalert the platform Statistics page
	// reads alerts from: https, or http to a service name only.
	EnvAlertsURL = "CODESYNAPSE_ALERTS_URL"
)

// StartMetrics exports one cumulative metric stream every 15s when a URL is
// configured, and samples process health (runtime.go) for it. Stop flushes
// once with a five-second deadline, even after ctx ends.
func StartMetrics(ctx context.Context, logger *slog.Logger, job string, versions ...string) (stop func(), err error) {
	if os.Getenv(EnvMetricsURL) == "" {
		return func() {}, nil
	}
	stopExport, err := startExport(ctx, logger, job, versions...)
	if err != nil {
		return nil, err
	}
	sampleCtx, stopSampling := context.WithCancel(ctx)
	go runRuntimeMetrics(sampleCtx)
	return sync.OnceFunc(func() {
		stopSampling()
		stopExport()
	}), nil
}

func startExport(ctx context.Context, logger *slog.Logger, job string, versions ...string) (stop func(), err error) {
	url := os.Getenv(EnvMetricsURL)
	if err := telemetry.CheckURL(url); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvMetricsURL, err)
	}
	exporter := Getenv(EnvMetricsExporter, "otlp")
	switch exporter {
	case "otlp":
		version := "unknown"
		if len(versions) > 0 && versions[0] != "" {
			version = versions[0]
		}
		provider, initErr := telemetry.NewOTLP(ctx, telemetry.OTLPConfig{
			URL: url, User: os.Getenv(EnvMetricsUser), Password: os.Getenv(EnvMetricsPassword),
			Service: "codesynapse-" + job, Instance: telemetry.InstanceID(), Version: version,
			Environment: Getenv(EnvEnvironment, "unknown"), Interval: 15 * time.Second, Timeout: 10 * time.Second,
		})
		if initErr != nil {
			return nil, fmt.Errorf("initializing metrics: %w", initErr)
		}
		logger.Info("metrics push enabled", "exporter", exporter, "job", job, "every", "15s")
		return sync.OnceFunc(func() {
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if shutdownErr := provider.Shutdown(final); shutdownErr != nil {
				logger.Warn("flushing metrics failed", "error", shutdownErr)
			}
		}), nil
	case "prometheus":
		return startPrometheusMetrics(ctx, logger, job, url), nil
	default:
		return nil, fmt.Errorf("%s must be otlp or prometheus", EnvMetricsExporter)
	}
}

func startPrometheusMetrics(ctx context.Context, logger *slog.Logger, job, url string) func() {
	p := &telemetry.Pusher{URL: url, User: os.Getenv(EnvMetricsUser), Password: os.Getenv(EnvMetricsPassword), Job: job, Instance: telemetry.InstanceID()}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx, 15*time.Second, func(err error) { logger.Warn("pushing metrics failed", "error", err) })
	}()
	logger.Info("metrics push enabled", "exporter", "prometheus", "job", job, "every", "15s")
	return sync.OnceFunc(func() {
		cancel()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			logger.Warn("metrics shutdown deadline exceeded")
		}
	})
}

// NewIngestMetrics uses the selected transport for fenced durable file totals.
// It returns nil when metrics are disabled and never starts its own export loop.
func NewIngestMetrics(ctx context.Context) (*telemetry.IngestExporter, error) {
	url := os.Getenv(EnvMetricsURL)
	if url == "" {
		return nil, nil
	}
	return telemetry.NewIngestExporter(ctx, telemetry.IngestExportConfig{
		URL: url, User: os.Getenv(EnvMetricsUser), Password: os.Getenv(EnvMetricsPassword),
		Format: Getenv(EnvMetricsExporter, "otlp"), Timeout: 10 * time.Second,
	})
}
