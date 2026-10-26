package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
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
	items := make([]SeriesBatchItem, len(names))
	_ = forEach(r.Context(), len(names), func(ctx context.Context, i int) error { //nolint:errcheck // each item carries its own status
		v := url.Values{"panel": {names[i]}}
		for _, k := range []string{"range", "repo"} {
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
