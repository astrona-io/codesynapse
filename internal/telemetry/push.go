package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Pusher sends a registry to VictoriaMetrics' Prometheus import endpoint.
// Pushing keeps the services free of a scrape listener; VictoriaMetrics
// stays on the internal network.
type Pusher struct {
	URL      string // e.g. http://victoriametrics:8428
	User     string
	Password string // never logged
	// Job and Instance become the job and instance labels.
	Job, Instance string
	Registry      *Registry
	HTTP          *http.Client
}

// CheckURL validates a VictoriaMetrics base URL.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("metrics url must be http(s)://host:port without credentials")
	}
	return nil
}

// Push sends the current values once.
func (p *Pusher) Push(ctx context.Context) error {
	reg := p.Registry
	if reg == nil {
		reg = Default
	}
	var body bytes.Buffer
	if err := reg.WriteText(&body); err != nil {
		return err
	}
	if body.Len() == 0 {
		return nil
	}
	q := url.Values{}
	q.Add("extra_label", "job="+p.Job)
	q.Add("extra_label", "instance="+p.Instance)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/api/v1/import/prometheus?"+q.Encode(), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	if p.User != "" {
		req.SetBasicAuth(p.User, p.Password)
	}
	client := p.HTTP
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
		return fmt.Errorf("metrics push: HTTP %d", res.StatusCode)
	}
	return nil
}

// Run pushes every interval until ctx ends, then once more so the last
// counts are not lost. onErr hears about failures (at most one a minute).
func (p *Pusher) Run(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	var lastErr time.Time
	report := func(err error) {
		if err != nil && time.Since(lastErr) > time.Minute {
			lastErr = time.Now()
			onErr(err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			report(p.Push(final))
			cancel()
			return
		case <-t.C:
			report(p.Push(ctx))
		}
	}
}
