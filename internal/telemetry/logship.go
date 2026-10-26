package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Bounds of the log shipper. Logging never waits on the network: lines that
// do not fit are dropped and counted.
const (
	logQueueBytes = 4 << 20
	logBatchBytes = 1 << 20
	logLineBytes  = 64 << 10
	logEvery      = 2 * time.Second
)

var logLinesDropped = Default.Counter("platform_log_lines_dropped_total", "Log lines not shipped to the log store.", "reason")

// LogShipper is the io.Writer of a slog JSON handler: every Write is one
// record, queued and sent in batches to VictoriaLogs' JSON lines endpoint.
// The lines are the ones the service writes to stderr anyway.
type LogShipper struct {
	URL      string // base URL, e.g. http://victorialogs:9428
	User     string
	Password string // never logged
	// StreamFields name the record fields that form a log stream (few
	// values each, e.g. service and instance).
	StreamFields []string
	HTTP         *http.Client

	mu      sync.Mutex
	queue   [][]byte
	size    int
	wake    chan struct{}
	initOne sync.Once
}

func (s *LogShipper) init() { s.initOne.Do(func() { s.wake = make(chan struct{}, 1) }) }

// Write queues one line. It never blocks on the network and never fails, so
// a slow or missing log store cannot hold up the service.
func (s *LogShipper) Write(p []byte) (int, error) {
	s.init()
	if len(p) > logLineBytes {
		logLinesDropped.Inc("too_long")
		return len(p), nil
	}
	s.mu.Lock()
	if s.size+len(p) > logQueueBytes {
		s.mu.Unlock()
		logLinesDropped.Inc("queue_full")
		return len(p), nil
	}
	s.queue = append(s.queue, bytes.Clone(p))
	s.size += len(p)
	full := s.size >= logBatchBytes
	s.mu.Unlock()
	if full {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

// take removes up to logBatchBytes of queued lines.
func (s *LogShipper) take() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b bytes.Buffer
	n := 0
	for n < len(s.queue) && (n == 0 || b.Len()+len(s.queue[n]) <= logBatchBytes) {
		line := s.queue[n]
		b.Write(line)
		if len(line) == 0 || line[len(line)-1] != '\n' {
			b.WriteByte('\n')
		}
		s.size -= len(line)
		n++
	}
	s.queue = s.queue[n:]
	return b.Bytes()
}

// Flush sends everything queued so far. A batch that fails is dropped and
// counted: retrying would only grow the queue while the store is away.
func (s *LogShipper) Flush(ctx context.Context) error {
	s.init()
	for {
		body := s.take()
		if len(body) == 0 {
			return nil
		}
		if err := s.send(ctx, body); err != nil {
			logLinesDropped.AddContext(ctx, float64(bytes.Count(body, []byte{'\n'})), "send_failed")
			return err
		}
	}
}

func (s *LogShipper) send(ctx context.Context, body []byte) error {
	q := url.Values{}
	q.Set("_msg_field", "msg")
	q.Set("_time_field", "time")
	if len(s.StreamFields) > 0 {
		q.Set("_stream_fields", strings.Join(s.StreamFields, ","))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.URL, "/")+"/insert/jsonline?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/stream+json")
	if s.User != "" {
		req.SetBasicAuth(s.User, s.Password)
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()                                      //nolint:errcheck // read-only body
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16)) //nolint:errcheck // drained for keep-alive
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("log push: HTTP %d", res.StatusCode)
	}
	return nil
}

// Run sends queued lines every two seconds, or sooner when a batch is full,
// until ctx ends. onErr hears about failures at most once a minute; it must
// not log through this shipper's handler, or a failing store feeds itself.
func (s *LogShipper) Run(ctx context.Context, onErr func(error)) {
	s.init()
	t := time.NewTicker(logEvery)
	defer t.Stop()
	var lastErr time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
		if err := s.Flush(ctx); err != nil && time.Since(lastErr) > time.Minute {
			lastErr = time.Now()
			onErr(err)
		}
	}
}
