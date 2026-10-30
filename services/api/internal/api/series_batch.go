package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// A page of charts (Live activity has 22) asked for each panel on its own,
// and every request authenticates afresh: about eight accounts queries for
// one chart. The batch answers many panels from one request, each exactly
// as GET /api/v1/stats/series would, status and errors included.

// maxBatchPanels bounds one batch.
const maxBatchPanels = 32

// SeriesBatchItem is one panel's answer: its status and, for 200, the panel,
// otherwise the problem document.
type SeriesBatchItem struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func (s *Server) handleSeriesBatch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var names []string
	for n := range strings.SplitSeq(q.Get("panels"), ",") {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 || len(names) > maxBatchPanels {
		s.problem(w, http.StatusBadRequest, errors.New("panels must name 1 to 32 panels, comma separated"))
		return
	}
	// Platform panels of one batch are one view: authorised and recorded
	// once, or each refused on its own below.
	var platform []string
	for _, n := range names {
		if seriesPanels[n].platform {
			platform = append(platform, n)
		}
	}
	ctx := r.Context()
	if len(platform) > 0 && s.cfg.Accounts != nil && s.cfg.Metrics != nil {
		if _, ok := seriesRanges[cmp.Or(q.Get("range"), "24h")]; ok {
			if p, ok := principal(ctx); ok {
				if _, err := s.cfg.Accounts.PlatformStats(ctx, p, accounts.StatsRequest{Section: "series", Panel: strings.Join(platform, ","), Range: cmp.Or(q.Get("range"), "24h")}); err == nil {
					ctx = context.WithValue(ctx, platformAuditedKey{}, true)
				}
			}
		}
	}
	items := make([]SeriesBatchItem, len(names))
	_ = forEach(ctx, len(names), func(ctx context.Context, i int) error { //nolint:errcheck // each item carries its own status
		v := url.Values{"panel": {names[i]}}
		for _, k := range []string{"range", "repo", "provider"} {
			if q.Has(k) {
				v.Set(k, q.Get(k))
			}
		}
		sub := r.Clone(ctx)
		sub.URL = &url.URL{Path: "/api/v1/stats/series", RawQuery: v.Encode()}
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		s.handleSeries(rec, sub)
		items[i] = SeriesBatchItem{Status: rec.status, Body: json.RawMessage(bytes.TrimSpace(rec.body.Bytes()))}
		return nil
	})
	out := make(map[string]SeriesBatchItem, len(names))
	for i, n := range names {
		out[n] = items[i]
	}
	writeJSON(w, http.StatusOK, map[string]any{"panels": out})
}

// bufferedResponse keeps one panel's answer in memory.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }
