package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Querier reads time series from VictoriaMetrics' Prometheus query API.
type Querier struct {
	URL      string
	User     string
	Password string // never logged
	HTTP     *http.Client
}

// Series is one labelled time series.
type Series struct {
	Labels map[string]string
	Points []Point
}

// Point is a sample; NaN-free (missing samples are absent).
type Point struct {
	T int64 // unix seconds
	V float64
}

// ErrBadTenant rejects tenant filters that are not organisation IDs.
var ErrBadTenant = errors.New("telemetry: tenant filter must be organisation IDs")

// ErrBadRepository rejects repository names and absent tenant scope.
var ErrBadRepository = errors.New("telemetry: repository filter requires an opaque repository ID and tenant scope")

// QueryRange runs a MetricsQL range query. tenants, when not nil, restricts
// every series to those tenant_id values through VictoriaMetrics'
// extra_filters, so the database enforces the scope whatever the query
// says; an empty non-nil list matches nothing.
func (q *Querier) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration, tenants []string) ([]Series, error) {
	return q.queryRange(ctx, expr, start, end, step, tenants, "")
}

// QueryRangeForRepo intersects a trusted repository-entry ID with tenant scope.
func (q *Querier) QueryRangeForRepo(ctx context.Context, expr string, start, end time.Time, step time.Duration, tenants []string, repositoryID string) ([]Series, error) {
	if tenants == nil || !tenantRe.MatchString(repositoryID) {
		return nil, ErrBadRepository
	}
	return q.queryRange(ctx, expr, start, end, step, tenants, repositoryID)
}

func (q *Querier) queryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration, tenants []string, repositoryID string) ([]Series, error) {
	v := url.Values{}
	v.Set("query", expr)
	v.Set("start", strconv.FormatInt(start.Unix(), 10))
	v.Set("end", strconv.FormatInt(end.Unix(), 10))
	v.Set("step", strconv.FormatInt(int64(step/time.Second), 10)+"s")
	if tenants != nil {
		if len(tenants) == 0 {
			return nil, nil
		}
		for _, t := range tenants {
			if !tenantRe.MatchString(t) {
				return nil, ErrBadTenant
			}
		}
		filter := `tenant_id=~"` + strings.Join(tenants, "|") + `"`
		if repositoryID != "" {
			filter += `,repo="` + repositoryID + `"`
		}
		v.Add("extra_filters[]", "{"+filter+"}")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(q.URL, "/")+"/api/v1/query_range?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if q.User != "" {
		req.SetBasicAuth(q.User, q.Password)
	}
	client := q.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close() //nolint:errcheck // read-only body
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	var env struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("metrics query: HTTP %d, unreadable answer", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK || env.Status != "success" {
		return nil, fmt.Errorf("metrics query: HTTP %d: %s", res.StatusCode, env.Error)
	}
	out := make([]Series, 0, len(env.Data.Result))
	for _, r := range env.Data.Result {
		s := Series{Labels: r.Metric, Points: make([]Point, 0, len(r.Values))}
		for _, pv := range r.Values {
			ts, ok := pv[0].(float64)
			str, _ := pv[1].(string)
			f, err := strconv.ParseFloat(str, 64)
			if !ok || err != nil || f != f { // f != f: NaN
				continue
			}
			s.Points = append(s.Points, Point{T: int64(ts), V: f})
		}
		out = append(out, s)
	}
	return out, nil
}
