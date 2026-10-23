package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// OTLPConfig exports cumulative metrics to an internal VictoriaMetrics base URL.
type OTLPConfig struct {
	URL, User, Password                     string
	Service, Instance, Version, Environment string
	Registry                                *Registry
	Interval, Timeout                       time.Duration
	HTTP                                    *http.Client
}

// InstanceID identifies this metric-producing process, not a chat session.
func InstanceID() string {
	var id [16]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read always fills the slice or terminates.
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}

// NewOTLP starts one bounded periodic exporter. Call Shutdown with a deadline.
// Registry recording never performs network I/O, including during outages.
func NewOTLP(ctx context.Context, cfg OTLPConfig) (*sdkmetric.MeterProvider, error) {
	if err := CheckURL(cfg.URL); err != nil {
		return nil, err
	}
	if cfg.Registry == nil {
		cfg.Registry = Default
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	cfg.Timeout = min(cfg.Timeout, 30*time.Second)
	if cfg.Service == "" {
		return nil, fmt.Errorf("metrics service name is required")
	}
	if cfg.Instance == "" {
		cfg.Instance = InstanceID()
	}
	if cfg.Version == "" {
		cfg.Version = "unknown"
	}
	if cfg.Environment == "" {
		cfg.Environment = "unknown"
	}
	exporter, err := newOTLPExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	reader := sdkmetric.NewPeriodicReader(&firstValueExporter{Exporter: exporter, seen: map[seriesIdentity]bool{}}, sdkmetric.WithProducer(cfg.Registry), sdkmetric.WithInterval(cfg.Interval), sdkmetric.WithTimeout(cfg.Timeout), sdkmetric.WithMaxExportBatchSize(512))
	res := resource.NewSchemaless(
		attribute.String("service.namespace", "codesynapse"), attribute.String("service.name", cfg.Service),
		attribute.String("service.instance.id", cfg.Instance), attribute.String("service.version", cfg.Version),
		attribute.String("deployment.environment.name", cfg.Environment),
		attribute.String("job", cfg.Service), attribute.String("instance", cfg.Instance),
	)
	return sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader)), nil
}

// newOTLPExporter shares transport and retry limits across process and durable streams.
func newOTLPExporter(ctx context.Context, cfg OTLPConfig) (sdkmetric.Exporter, error) {
	client := http.Client{Timeout: cfg.Timeout}
	if cfg.HTTP != nil {
		client = *cfg.HTTP
		client.Timeout = cfg.Timeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	headers := map[string]string{}
	if cfg.User != "" {
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.User+":"+cfg.Password))
	}
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(strings.TrimRight(cfg.URL, "/")+"/opentelemetry/v1/metrics"),
		otlpmetrichttp.WithHTTPClient(&client), otlpmetrichttp.WithHeaders(headers),
		otlpmetrichttp.WithCompression(otlpmetrichttp.NoCompression),
		otlpmetrichttp.WithTemporalitySelector(func(sdkmetric.InstrumentKind) metricdata.Temporality { return metricdata.CumulativeTemporality }),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: true, InitialInterval: 200 * time.Millisecond, MaxInterval: time.Second, MaxElapsedTime: cfg.Timeout}),
	)
	if err != nil {
		return nil, fmt.Errorf("initializing OTLP metrics: %w", err)
	}
	return exporter, nil
}
