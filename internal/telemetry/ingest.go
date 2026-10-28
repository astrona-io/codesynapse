package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	// MaxIngestRepositories admits at most 4,800 tenant/repository/reason series.
	MaxIngestRepositories = 800
	// MaxTenantIngestRepositories bounds one tenant's share of the durable registry.
	MaxTenantIngestRepositories = 64
	ingestService               = "codesynapse-ingest-accounting"
	ingestSchemaVersion         = "1"
	maxIngestCounter            = int64(1 << 53)
)

var ingestReasons = []string{"ignored_path", "generated_code", "binary", "non_k8s_yaml"}

// IngestTotal comes only from the tenant-owned ledger's admitted repository entry.
// RepositoryID is its permanent opaque ID, never a graph ID or repository name.
type IngestTotal struct {
	TenantID, RepositoryID string
	StartedAt              time.Time
	Scanned, Written       int64
	Skipped                map[string]int64
}

// IngestSnapshot is read and exported while the accounts store holds its export fence.
// All identifiers, timestamps and totals come from that store, never request arguments.
type IngestSnapshot struct {
	StreamID            string
	StartedAt, SampleAt time.Time
	Totals              []IngestTotal
	DroppedSeries       int64
}

// IngestExportConfig selects the same receiver and protocol as ordinary telemetry.
// Resource identity is fixed by the durable stream, independent of process/version.
type IngestExportConfig struct {
	URL, User, Password, Format string
	Timeout                     time.Duration
	HTTP                        *http.Client
}

type ingestAdmission struct {
	tenant  string
	started time.Time
	values  [6]int64
}

// IngestExporter sends absolute durable totals; it never replays counter increments.
// Its caller must retain the store fence through Export. Shutdown never flushes.
type IngestExporter struct {
	mu            sync.Mutex
	cfg           IngestExportConfig
	exporter      sdkmetric.Exporter
	http          *http.Client
	stream        string
	started, last time.Time
	dropped       int64
	admitted      map[string]ingestAdmission
	closed        bool
}

// NewIngestExporter initializes one transport without starting a background reader.
func NewIngestExporter(ctx context.Context, cfg IngestExportConfig) (*IngestExporter, error) {
	if err := CheckURL(cfg.URL); err != nil {
		return nil, err
	}
	if cfg.Format == "" {
		cfg.Format = "otlp"
	}
	if cfg.Format != "otlp" && cfg.Format != "prometheus" {
		return nil, errors.New("ingest metrics exporter must be otlp or prometheus")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	cfg.Timeout = min(cfg.Timeout, 30*time.Second)
	client := http.Client{Timeout: cfg.Timeout}
	if cfg.HTTP != nil {
		client = *cfg.HTTP
		client.Timeout = cfg.Timeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	e := &IngestExporter{cfg: cfg, http: &client, admitted: map[string]ingestAdmission{}}
	if cfg.Format == "otlp" {
		x, err := newOTLPExporter(ctx, OTLPConfig{URL: cfg.URL, User: cfg.User, Password: cfg.Password, Timeout: cfg.Timeout, HTTP: &client})
		if err != nil {
			return nil, err
		}
		e.exporter = x
	}
	return e, nil
}

// Export validates the complete snapshot before admitting labels or sending data.
// Failed exports leave ledger totals intact; retrying the same snapshot is safe.
func (e *IngestExporter) Export(ctx context.Context, s IngestSnapshot) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("ingest metrics exporter is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if reason := e.validate(ctx, s); reason != "" {
		ingestExportRejections.IncContext(ctx, reason)
		return errors.New("ingest metric snapshot rejected: " + reason)
	}
	e.admit(s)
	ctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()
	if e.exporter != nil {
		data := ingestMetricData(s)
		if err := e.exporter.Export(ctx, &data); err != nil {
			ExportFailed(ctx, ExporterIngest)
			return exportError(ctx)
		}
		return nil
	}
	if err := e.push(ctx, s); err != nil {
		ExportFailed(ctx, ExporterIngest)
		return err
	}
	return nil
}

func (e *IngestExporter) validate(ctx context.Context, s IngestSnapshot) string {
	if !tenantRe.MatchString(s.StreamID) || s.StartedAt.IsZero() || s.SampleAt.IsZero() || s.StartedAt.UnixMilli() < 1 || s.SampleAt.Before(s.StartedAt) || s.SampleAt.After(time.Now().Add(time.Minute)) || s.DroppedSeries < 0 || s.DroppedSeries > maxIngestCounter {
		return "invalid"
	}
	if e.stream != "" && (s.StreamID != e.stream || !s.StartedAt.Equal(e.started)) {
		return "scope"
	}
	if s.SampleAt.Before(e.last) || s.DroppedSeries < e.dropped || s.SampleAt.Equal(e.last) && s.DroppedSeries != e.dropped {
		return "stale"
	}
	if len(s.Totals) > MaxIngestRepositories {
		return "capacity"
	}
	counts := map[string]int{}
	for _, a := range e.admitted {
		counts[a.tenant]++
	}
	seen := map[string]bool{}
	additional := 0
	for _, row := range s.Totals {
		if !tenantRe.MatchString(row.TenantID) || !tenantRe.MatchString(row.RepositoryID) || seen[row.RepositoryID] {
			return "scope"
		}
		if tenant := Tenant(ctx); tenantRe.MatchString(tenant) && tenant != row.TenantID {
			return "scope"
		}
		seen[row.RepositoryID] = true
		if row.StartedAt.IsZero() || row.StartedAt.Before(s.StartedAt) || row.StartedAt.After(s.SampleAt) || row.Scanned < 0 || row.Scanned > maxIngestCounter || row.Written < 0 || row.Written > row.Scanned {
			return "invalid"
		}
		skipped := int64(0)
		for reason, count := range row.Skipped {
			if !slices.Contains(ingestReasons, reason) || count < 0 || count > row.Scanned-skipped {
				return "invalid"
			}
			skipped += count
		}
		if skipped > row.Scanned-row.Written {
			return "invalid"
		}
		if old, ok := e.admitted[row.RepositoryID]; ok {
			if old.tenant != row.TenantID || !old.started.Equal(row.StartedAt) {
				return "scope"
			}
			for i, value := range ingestValues(row) {
				if value < old.values[i] || s.SampleAt.Equal(e.last) && value != old.values[i] {
					return "stale"
				}
			}
		} else {
			if s.SampleAt.Equal(e.last) {
				return "stale"
			}
			additional++
			counts[row.TenantID]++
			if counts[row.TenantID] > MaxTenantIngestRepositories {
				return "capacity"
			}
		}
	}
	if len(e.admitted)+additional > MaxIngestRepositories {
		return "capacity"
	}
	return ""
}
func ingestValues(row IngestTotal) [6]int64 {
	return [6]int64{row.Scanned, row.Written, row.Skipped["ignored_path"], row.Skipped["generated_code"], row.Skipped["binary"], row.Skipped["non_k8s_yaml"]}
}
func (e *IngestExporter) admit(s IngestSnapshot) {
	e.stream, e.started, e.last, e.dropped = s.StreamID, s.StartedAt, s.SampleAt, s.DroppedSeries
	for _, row := range s.Totals {
		e.admitted[row.RepositoryID] = ingestAdmission{row.TenantID, row.StartedAt, ingestValues(row)}
	}
}

func ingestMetricData(s IngestSnapshot) metricdata.ResourceMetrics {
	names := []string{FilesScannedMetric, FilesIndexedMetric, FilesSkippedMetric, IngestDroppedSeriesMetric}
	descriptions := []string{"File entries encountered across ingestion runs.", "File contributions written by committed ingestion runs; excludes reuse.", "Whole-file skips across ingestion runs by approved reason.", "Counter series denied by durable repository admission limits."}
	points := make([][]metricdata.DataPoint[int64], len(names))
	appendPoints := func(index int, attrs attribute.Set, start time.Time, value int64) {
		baseline := start.Add(-time.Millisecond)
		points[index] = append(points[index], metricdata.DataPoint[int64]{Attributes: attrs, StartTime: baseline, Time: baseline, Value: 0}, metricdata.DataPoint[int64]{Attributes: attrs, StartTime: start, Time: s.SampleAt, Value: value})
	}
	for _, row := range s.Totals {
		attrs := []attribute.KeyValue{attribute.String("tenant_id", row.TenantID), attribute.String("repo", row.RepositoryID), attribute.String("mcp_tool", "none"), attribute.String("llm_provider", "none"), attribute.String("llm_model", "none")}
		appendPoints(0, attribute.NewSet(attrs...), row.StartedAt, row.Scanned)
		appendPoints(1, attribute.NewSet(attrs...), row.StartedAt, row.Written)
		for _, reason := range ingestReasons {
			appendPoints(2, attribute.NewSet(append(slices.Clone(attrs), attribute.String("skip_reason", reason))...), row.StartedAt, row.Skipped[reason])
		}
	}
	appendPoints(3, attribute.NewSet(), s.StartedAt, s.DroppedSeries)
	scope := metricdata.ScopeMetrics{Scope: instrumentation.Scope{Name: "codesynapse/ingest-accounting", Version: ingestSchemaVersion}}
	for i, name := range names {
		if len(points[i]) > 0 {
			scope.Metrics = append(scope.Metrics, metricdata.Metrics{Name: name, Description: descriptions[i], Unit: "1", Data: metricdata.Sum[int64]{Temporality: metricdata.CumulativeTemporality, IsMonotonic: true, DataPoints: points[i]}})
		}
	}
	// Application build versions belong to process metrics; changing them must not fork durable counters.
	res := resource.NewSchemaless(attribute.String("service.namespace", "codesynapse"), attribute.String("service.name", ingestService), attribute.String("service.version", ingestSchemaVersion), attribute.String("service.instance.id", s.StreamID), attribute.String("job", ingestService), attribute.String("instance", s.StreamID))
	return metricdata.ResourceMetrics{Resource: res, ScopeMetrics: []metricdata.ScopeMetrics{scope}}
}

func (e *IngestExporter) push(ctx context.Context, s IngestSnapshot) error {
	var body bytes.Buffer
	data := ingestMetricData(s)
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			sum := metric.Data.(metricdata.Sum[int64])
			for _, p := range sum.DataPoints {
				labels := []string{}
				for _, a := range p.Attributes.ToSlice() {
					labels = append(labels, string(a.Key)+"="+strconv.Quote(a.Value.AsString()))
				}
				if _, err := fmt.Fprintf(&body, "%s{%s} %d %d\n", metric.Name, strings.Join(labels, ","), p.Value, p.Time.UnixMilli()); err != nil {
					return err
				}
			}
		}
	}
	q := url.Values{}
	for _, a := range data.Resource.Attributes() {
		q.Add("extra_label", string(a.Key)+"="+a.Value.AsString())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.cfg.URL, "/")+"/api/v1/import/prometheus?"+q.Encode(), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	if e.cfg.User != "" {
		req.SetBasicAuth(e.cfg.User, e.cfg.Password)
	}
	res, err := e.http.Do(req)
	if err != nil {
		return exportError(ctx)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode/100 != 2 {
		return exportError(ctx)
	}
	return nil
}

// Shutdown closes transport resources without exporting outside the durable fence.
func (e *IngestExporter) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	e.http.CloseIdleConnections()
	if e.exporter != nil {
		if err := e.exporter.Shutdown(ctx); err != nil {
			return exportError(ctx)
		}
	}
	return nil
}
