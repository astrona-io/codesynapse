package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the API image has no zoneinfo; peak hours need the viewer's zone

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// Model queue statistics (docs/operations/local-models.md): how long model
// requests wait for a provider's queue slot, and when. The charts are
// ordinary panels; the peak hours are folded here from 15-minute steps into
// weekday x time-of-day cells in the viewer's time zone, which never reaches
// a query: the zone only decides which cell a step lands in.

// Metric selectors. Organisations read their own series (the tenant comes
// from extra_filters); platform panels also read the platform_ twins of
// work without a tenant.
const (
	queueBucketsOrg      = `llm_queue_wait_seconds_bucket`
	queueSumOrg          = `llm_queue_wait_seconds_sum`
	queueRunningOrg      = `llm_queue_running`
	queueWaitingOrg      = `llm_queue_waiting`
	queueStartOrg        = `llm_answer_start_seconds_bucket`
	queueBucketsPlatform = `{__name__=~"(platform_)?llm_queue_wait_seconds_bucket"}`
	queueSumPlatform     = `{__name__=~"(platform_)?llm_queue_wait_seconds_sum"}`
	queueRunningPlatform = `{__name__=~"(platform_)?llm_queue_running"}`
	queueWaitingPlatform = `{__name__=~"(platform_)?llm_queue_waiting"}`
	queueStartPlatform   = `{__name__=~"(platform_)?llm_answer_start_seconds_bucket"}`
	queueCapacity        = `platform_llm_queue_capacity`
)

// queueSelectors are the metrics one view reads.
type queueSelectors struct{ buckets, sum, running, waiting, start string }

var (
	orgQueue      = queueSelectors{queueBucketsOrg, queueSumOrg, queueRunningOrg, queueWaitingOrg, queueStartOrg}
	platformQueue = queueSelectors{queueBucketsPlatform, queueSumPlatform, queueRunningPlatform, queueWaitingPlatform, queueStartPlatform}
)

// queueFirstBound is the first bucket of llm_queue_wait_seconds: a request
// at or below it found a free slot.
const queueFirstBound = 0.005

// Every series name joins the provider and what the line is, so two lines
// of one provider never share a name.
func queueQuantile(sel string, q float64, name string) string {
	return fmt.Sprintf(`label_join(label_set(histogram_quantile(%g, sum by (le, llm_provider) (increase(%s[$w]))), "q", %q), "series", " ", "llm_provider", "q")`, q, sel, name)
}

func queuePanels(sel queueSelectors, platform bool) map[string]seriesPanel {
	who := "this organisation's"
	if platform {
		who = "every organisation's"
	}
	load := []seriesQuery{
		{name: "running", expr: `sum(max_over_time(` + sel.running + `[$w]))`},
		{name: "waiting", expr: `sum(max_over_time(` + sel.waiting + `[$w]))`},
	}
	loadHelp := "Most requests holding a queue slot and waiting for one in each step, " + who + ", summed over providers and processes."
	if platform {
		load = append(load, seriesQuery{name: "slots", expr: `sum(max_over_time(` + queueCapacity + `[$w]))`})
		loadHelp += " Slots is max_concurrent summed over the processes that run model requests: running at slots means the endpoint is full."
	}
	panels := map[string]seriesPanel{
		"queue_wait": {Title: "Model queue wait", Unit: "seconds", Kind: "gauge",
			Help:  "Median and 95th percentile time " + who + " model requests waited for a queue slot, by provider.",
			Empty: "No model request in this window.",
			queries: []seriesQuery{
				{expr: queueQuantile(sel.buckets, 0.5, "p50"), by: "series"},
				{expr: queueQuantile(sel.buckets, 0.95, "p95"), by: "series"},
			}},
		"queue_saturation": {Title: "Requests that found the model busy", Unit: "%", Kind: "gauge",
			Help:    "Share of " + who + " model requests that found every queue slot taken and had to wait, by provider.",
			Empty:   "No model request in this window.",
			queries: []seriesQuery{{expr: fmt.Sprintf(`100 * (1 - histogram_share(%g, sum by (le, llm_provider) (increase(%s[$w]))))`, queueFirstBound, sel.buckets), by: "llm_provider"}}},
		"queue_load": {Title: "Running and waiting model requests", Unit: "requests", Kind: "gauge",
			Help:    loadHelp,
			Empty:   "No model request in this window.",
			queries: load},
		"queue_start": {Title: "Time until the model starts answering", Unit: "seconds", Kind: "gauge",
			Help:    "95th percentile from the queue slot to the first streamed token (first_token), or to the whole answer where the provider does not stream (full_answer), by provider.",
			Empty:   "No model answer in this window.",
			queries: []seriesQuery{{expr: `label_join(histogram_quantile(0.95, sum by (le, llm_provider, basis) (increase(` + sel.start + `[$w]))), "series", " ", "llm_provider", "basis")`, by: "series"}}},
	}
	// Every queue series carries llm_provider (capacity included), so a
	// model's tab can ask for its own lines only.
	for name, p := range panels {
		p.byProvider = true
		panels[name] = p
	}
	return panels
}

func init() {
	for name, p := range queuePanels(orgQueue, false) {
		seriesPanels[name] = p
	}
	// Providers are the installation's own configuration and the values
	// are the endpoint's, so these are not split by organisation.
	for name, p := range queuePanels(platformQueue, true) {
		adminPanels[name] = adminPanel{seriesPanel: p}
	}
}

// Peak hours.
const (
	queueSlotMinutes = 15
	queueSlotsPerDay = 24 * 60 / queueSlotMinutes
	queueCells       = 7 * queueSlotsPerDay
	// maxQueueProviders bounds the answer; the busiest providers stay.
	maxQueueProviders = 8
	maxTimezoneName   = 64
)

// queueWeeks are the selectable spans and the audit range each is recorded
// under (the smallest statistics range that covers it).
var queueWeeks = map[int]string{1: "7d", 4: "30d", 12: "90d"}

// Capacity verdict thresholds, returned with the answer so the page can
// explain them.
const (
	verdictMinRequests  = 20  // fewer requests in the span: not enough to judge
	peakMinRequests     = 3   // a cell needs this many to be a peak
	verdictWaitHigh     = 10. // seconds at p95 ...
	verdictShareHigh    = 0.5 // ... and this share found it full: add an endpoint
	verdictWaitNotable  = 3.  // seconds at p95, or ...
	verdictShareNotable = 0.25
)

// QueuePeak is the model queue page: load now, peak hours and a verdict per
// provider.
type QueuePeak struct {
	Scope       string          `json:"scope"`
	Weeks       int             `json:"weeks"`
	Timezone    string          `json:"timezone"`
	SlotMinutes int             `json:"slot_minutes"`
	Providers   []QueueProvider `json:"providers"`
	Thresholds  QueueThresholds `json:"thresholds"`
}

// QueueThresholds are the verdict constants.
type QueueThresholds struct {
	MinRequests     int     `json:"min_requests"`
	PeakMinRequests int     `json:"peak_min_requests"`
	WaitHigh        float64 `json:"wait_high_seconds"`
	ShareHigh       float64 `json:"share_high"`
	WaitNoticeable  float64 `json:"wait_noticeable_seconds"`
	ShareNoticeable float64 `json:"share_noticeable"`
	FreeSlotSeconds float64 `json:"free_slot_seconds"`
}

// QueueProvider is one provider's queue.
type QueueProvider struct {
	Name string `json:"name"`
	// Configured: the provider is in the installation's model
	// configuration, so its charts can be asked for on their own
	// (?provider=). A provider only the measurements still know is not.
	Configured bool `json:"configured"`
	// Models are the configured chat and embedding models.
	Models []string `json:"models"`
	// Slots is max_concurrent in each process (0: unlimited or not
	// configured here); Capacity is the slots summed over the processes
	// that reported, platform view only.
	Slots    int      `json:"slots"`
	Capacity *float64 `json:"capacity,omitempty"`
	Running  *float64 `json:"running"`
	Waiting  *float64 `json:"waiting"`
	// Cells are weekday (Monday first) x 15-minute slot; nil has no
	// measurements, which is not the same as no requests.
	Cells    []*QueueCell `json:"cells"`
	Requests float64      `json:"requests"`
	// P95 and Share are over the whole period: the 95th percentile wait
	// and the share of requests that found every slot taken. Nil without
	// requests.
	P95     *float64     `json:"p95"`
	Share   *float64     `json:"share"`
	Verdict QueueVerdict `json:"verdict"`
}

// QueueCell sums every step that fell into one weekday and slot. Avg, P95
// and Share are nil when no request came.
type QueueCell struct {
	Requests float64  `json:"requests"`
	Avg      *float64 `json:"avg"`
	P95      *float64 `json:"p95"`
	Share    *float64 `json:"share"`
}

// QueueVerdict says whether another endpoint would help.
type QueueVerdict struct {
	// Level is unknown, fine, noticeable or add_endpoint.
	Level string `json:"level"`
	Text  string `json:"text"`
	// Peak is the busiest stretch: a weekday and the slots it spans.
	Weekday   int      `json:"weekday"`
	FirstSlot int      `json:"first_slot"`
	LastSlot  int      `json:"last_slot"`
	P95       *float64 `json:"p95"`
	Share     *float64 `json:"share"`
}

func (s *Server) handleQueuePeak(w http.ResponseWriter, r *http.Request) {
	weeks, loc, ok := s.queueParams(w, r)
	if !ok {
		return
	}
	s.writeQueuePeak(r.Context(), w, "org", weeks, loc, orgQueue, callerTenants(r))
}

func (s *Server) handleAdminQueuePeak(w http.ResponseWriter, r *http.Request) {
	weeks, loc, ok := s.queueParams(w, r)
	if !ok {
		return
	}
	sq, ok := s.platformView(w, r, accounts.StatsRequest{Section: "series", Panel: "queue_peak", Range: queueWeeks[weeks], OrgID: r.URL.Query().Get("org")})
	if !ok {
		return
	}
	var tenants []string // nil: every tenant
	if sq.OrgID != "" {
		tenants = []string{sq.OrgID}
	}
	s.writeQueuePeak(r.Context(), w, "platform", weeks, loc, platformQueue, tenants)
}

func (s *Server) queueParams(w http.ResponseWriter, r *http.Request) (int, *time.Location, bool) {
	if s.cfg.Metrics == nil {
		s.problem(w, http.StatusConflict, errors.New("metrics are not configured (CODESYNAPSE_METRICS_URL)"))
		return 0, nil, false
	}
	weeks := 4
	if v := r.URL.Query().Get("weeks"); v != "" {
		n, err := strconv.Atoi(v)
		if _, ok := queueWeeks[n]; err != nil || !ok {
			s.problem(w, http.StatusBadRequest, errors.New("weeks must be 1, 4 or 12"))
			return 0, nil, false
		}
		weeks = n
	}
	return weeks, viewerZone(r.URL.Query().Get("tz")), true
}

// viewerZone is the IANA zone the browser named, or UTC for anything
// LoadLocation does not know. "Local" would be the server's zone.
func viewerZone(name string) *time.Location {
	if name == "" || name == "Local" || len(name) > maxTimezoneName {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

func (s *Server) writeQueuePeak(ctx context.Context, w http.ResponseWriter, scope string, weeks int, loc *time.Location, sel queueSelectors, tenants []string) {
	out, err := s.queuePeak(ctx, scope, weeks, time.Now(), loc, sel, tenants)
	if err != nil {
		s.problem(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) queuePeak(ctx context.Context, scope string, weeks int, now time.Time, loc *time.Location, sel queueSelectors, tenants []string) (QueuePeak, error) {
	step := queueSlotMinutes * time.Minute
	// Whole slots only: the step ending at end covers (end-15m, end].
	end := now.Truncate(step)
	week := 7 * 24 * time.Hour
	// One query per week keeps each answer far below the querier's limit
	// (a bucket series of twelve weeks would be 8064 points).
	bucketsQ := `sum by (le, llm_provider) (increase(` + sel.buckets + `[15m]))`
	sumQ := `sum by (llm_provider) (increase(` + sel.sum + `[15m]))`
	type part struct{ buckets, sums []telemetry.Series }
	parts := make([]part, weeks)
	err := forEach(ctx, weeks, func(ctx context.Context, i int) error {
		to := end.Add(-time.Duration(i) * week)
		from := to.Add(-week).Add(step)
		b, err := s.cfg.Metrics.QueryRange(ctx, bucketsQ, from, to, step, tenants)
		if err != nil {
			return err
		}
		sums, err := s.cfg.Metrics.QueryRange(ctx, sumQ, from, to, step, tenants)
		if err != nil {
			return err
		}
		parts[i] = part{b, sums}
		return nil
	})
	if err != nil {
		return QueuePeak{}, err
	}
	var buckets, sums []telemetry.Series
	for _, p := range parts {
		buckets = append(buckets, p.buckets...)
		sums = append(sums, p.sums...)
	}
	folded := foldQueue(buckets, sums, step, loc)

	nowQ := func(expr string) (map[string]float64, error) {
		res, err := s.cfg.Metrics.QueryRange(ctx, `sum by (llm_provider) (`+expr+`)`, now.Add(-2*time.Minute), now, time.Minute, tenants)
		if err != nil {
			return nil, err
		}
		latest := map[string]float64{}
		for _, ts := range res {
			if n := len(ts.Points); n > 0 {
				latest[ts.Labels["llm_provider"]] = ts.Points[n-1].V
			}
		}
		return latest, nil
	}
	running, err := nowQ(sel.running)
	if err != nil {
		return QueuePeak{}, err
	}
	waiting, err := nowQ(sel.waiting)
	if err != nil {
		return QueuePeak{}, err
	}
	var capacity map[string]float64
	if scope == "platform" {
		// The capacity carries no tenant, so it is read across the
		// installation: platform view only.
		if capacity, err = nowQ(queueCapacity); err != nil {
			return QueuePeak{}, err
		}
	}

	slotsOf := map[string]int{}
	modelsOf := map[string][]string{}
	if s.cfg.LLM != nil {
		for _, p := range s.cfg.LLM.Configs() {
			models := []string{}
			for _, m := range []string{p.Model, p.EmbeddingModel} {
				// "auto" before the server answered names no model yet.
				if m != "" && m != llm.Auto && !slices.Contains(models, m) {
					models = append(models, m)
				}
			}
			modelsOf[p.Name] = models
			if n := p.QueueSlots(); n > 0 {
				slotsOf[p.Name] = n
				if _, ok := folded[p.Name]; !ok {
					folded[p.Name] = &queueFold{}
				}
			}
		}
	}
	th := queueThresholds()
	out := QueuePeak{Scope: scope, Weeks: weeks, Timezone: loc.String(), SlotMinutes: queueSlotMinutes, Providers: []QueueProvider{}, Thresholds: th}
	for name, f := range folded {
		if name == "" || name == "none" {
			continue // a provider the telemetry allowlist did not know
		}
		models, configured := modelsOf[name]
		if models == nil {
			models = []string{}
		}
		p := QueueProvider{Name: name, Configured: configured, Models: models, Slots: slotsOf[name], Cells: f.publish(), Requests: f.requests()}
		p.P95, p.Share = f.overall()
		p.Running, p.Waiting = valueOf(running, name), valueOf(waiting, name)
		if capacity != nil {
			p.Capacity = valueOf(capacity, name)
		}
		p.Verdict = f.verdict()
		out.Providers = append(out.Providers, p)
	}
	slices.SortFunc(out.Providers, func(a, b QueueProvider) int {
		return cmp.Or(cmp.Compare(b.Requests, a.Requests), cmp.Compare(a.Name, b.Name))
	})
	if len(out.Providers) > maxQueueProviders {
		out.Providers = out.Providers[:maxQueueProviders]
	}
	return out, nil
}

func valueOf(m map[string]float64, name string) *float64 {
	if v, ok := m[name]; ok {
		return &v
	}
	return nil
}

func queueThresholds() QueueThresholds {
	return QueueThresholds{MinRequests: verdictMinRequests, PeakMinRequests: peakMinRequests,
		WaitHigh: verdictWaitHigh, ShareHigh: verdictShareHigh, WaitNoticeable: verdictWaitNotable, ShareNoticeable: verdictShareNotable,
		FreeSlotSeconds: queueFirstBound}
}

// queueAcc adds up the bucket increments and wait sums of steps.
type queueAcc struct {
	seen    bool
	buckets map[float64]float64 // le -> cumulative count
	sum     float64
}

func (a *queueAcc) merge(b *queueAcc) {
	if b == nil || !b.seen {
		return
	}
	a.seen = true
	if a.buckets == nil {
		a.buckets = map[float64]float64{}
	}
	for le, n := range b.buckets {
		a.buckets[le] += n
	}
	a.sum += b.sum
}

func (a *queueAcc) count() float64 { return a.buckets[math.Inf(1)] }

// quantile interpolates within the bucket like histogram_quantile.
func (a *queueAcc) quantile(q float64) (float64, bool) {
	total := a.count()
	if total <= 0 {
		return 0, false
	}
	les := make([]float64, 0, len(a.buckets))
	for le := range a.buckets {
		les = append(les, le)
	}
	slices.Sort(les)
	rank := q * total
	prevLe, prevN := 0.0, 0.0
	for _, le := range les {
		n := max(a.buckets[le], prevN) // increase() may round a bucket below the one before
		if n >= rank {
			if math.IsInf(le, 1) {
				return prevLe, true
			}
			if n == prevN {
				return le, true
			}
			return prevLe + (le-prevLe)*(rank-prevN)/(n-prevN), true
		}
		prevLe, prevN = le, n
	}
	return prevLe, true
}

// queued is the share of requests that waited past the first bucket.
func (a *queueAcc) queued() (float64, bool) {
	total := a.count()
	free, ok := a.buckets[queueFirstBound]
	if total <= 0 || !ok {
		return 0, false
	}
	return min(max(1-free/total, 0), 1), true
}

// queueFold is one provider's weekday x slot cells.
type queueFold struct{ cells [queueCells]*queueAcc }

func (f *queueFold) cell(i int) *queueAcc {
	if f.cells[i] == nil {
		f.cells[i] = &queueAcc{buckets: map[float64]float64{}}
	}
	return f.cells[i]
}

// cellOf places the step that ends at t (it covers the slot before t) in
// the viewer's zone: weekday Monday first, then the slot of the day. Across
// a daylight saving change the repeated hour lands twice in the same cells
// and the skipped hour not at all.
func cellOf(t int64, step time.Duration, loc *time.Location) int {
	at := time.Unix(t, 0).Add(-step).In(loc)
	day := (int(at.Weekday()) + 6) % 7
	return day*queueSlotsPerDay + (at.Hour()*60+at.Minute())/queueSlotMinutes
}

// foldQueue folds 15-minute steps of bucket and sum increments into cells
// per provider.
func foldQueue(buckets, sums []telemetry.Series, step time.Duration, loc *time.Location) map[string]*queueFold {
	out := map[string]*queueFold{}
	fold := func(provider string) *queueFold {
		f, ok := out[provider]
		if !ok {
			f = &queueFold{}
			out[provider] = f
		}
		return f
	}
	for _, ts := range buckets {
		le, err := strconv.ParseFloat(ts.Labels["le"], 64)
		if err != nil {
			continue
		}
		f := fold(ts.Labels["llm_provider"])
		for _, pt := range ts.Points {
			c := f.cell(cellOf(pt.T, step, loc))
			c.seen = true
			c.buckets[le] += max(pt.V, 0)
		}
	}
	for _, ts := range sums {
		f := fold(ts.Labels["llm_provider"])
		for _, pt := range ts.Points {
			c := f.cell(cellOf(pt.T, step, loc))
			c.seen = true
			c.sum += max(pt.V, 0)
		}
	}
	return out
}

func (f *queueFold) publish() []*QueueCell {
	out := make([]*QueueCell, queueCells)
	for i, c := range f.cells {
		if c == nil || !c.seen {
			continue
		}
		cell := &QueueCell{Requests: c.count()}
		if cell.Requests > 0 {
			avg := c.sum / cell.Requests
			cell.Avg = &avg
			if v, ok := c.quantile(0.95); ok {
				cell.P95 = &v
			}
			if v, ok := c.queued(); ok {
				cell.Share = &v
			}
		}
		out[i] = cell
	}
	return out
}

func (f *queueFold) requests() float64 {
	total := 0.0
	for _, c := range f.cells {
		if c != nil {
			total += c.count()
		}
	}
	return total
}

// overall is the p95 wait and the share that found the endpoint full over
// every cell; nil without requests.
func (f *queueFold) overall() (p95, share *float64) {
	var acc queueAcc
	for _, c := range f.cells {
		acc.merge(c)
	}
	if v, ok := acc.quantile(0.95); ok {
		p95 = &v
	}
	if v, ok := acc.queued(); ok {
		share = &v
	}
	return p95, share
}

var weekdayNames = [7]string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// verdict finds the busiest stretch of one weekday, the cell with the
// highest p95 wait widened over neighbours that also wait noticeably, and
// judges it by its p95 wait and the share of requests that found the
// endpoint full.
func (f *queueFold) verdict() QueueVerdict {
	total := f.requests()
	if total < verdictMinRequests {
		return QueueVerdict{Level: "unknown", Weekday: -1, FirstSlot: -1, LastSlot: -1,
			Text: fmt.Sprintf("Not enough model requests in this period to judge: %d of the %d needed.", int(total), verdictMinRequests)}
	}
	best, bestP95, bestShare := -1, -1.0, -1.0
	for i, c := range f.cells {
		if c == nil || c.count() < peakMinRequests {
			continue
		}
		p95, _ := c.quantile(0.95)
		share, _ := c.queued()
		if p95 > bestP95 || (p95 == bestP95 && share > bestShare) {
			best, bestP95, bestShare = i, p95, share
		}
	}
	if best < 0 {
		return QueueVerdict{Level: "fine", Weekday: -1, FirstSlot: -1, LastSlot: -1,
			Text: "Requests are spread thin: no 15-minute slot saw enough of them to show a peak. One endpoint is enough."}
	}
	day, first := best/queueSlotsPerDay, best%queueSlotsPerDay
	last := first
	notable := func(slot int) bool {
		c := f.cells[day*queueSlotsPerDay+slot]
		if c == nil || c.count() <= 0 {
			return false
		}
		p95, _ := c.quantile(0.95)
		share, _ := c.queued()
		return p95 >= verdictWaitNotable || share >= verdictShareNotable
	}
	if notable(first) {
		for first > 0 && notable(first-1) {
			first--
		}
		for last < queueSlotsPerDay-1 && notable(last+1) {
			last++
		}
	}
	var acc queueAcc
	for slot := first; slot <= last; slot++ {
		acc.merge(f.cells[day*queueSlotsPerDay+slot])
	}
	p95, _ := acc.quantile(0.95)
	share, _ := acc.queued()
	v := QueueVerdict{Weekday: day, FirstSlot: first, LastSlot: last, P95: &p95, Share: &share}
	when := fmt.Sprintf("%s %s–%s", weekdayNames[day], slotClock(first), slotClock(last+1))
	waits := fmt.Sprintf("requests wait %s at p95 and the endpoint is full for %d%% of them", seconds(p95), int(math.Round(share*100)))
	switch {
	case p95 >= verdictWaitHigh && share >= verdictShareHigh:
		v.Level = "add_endpoint"
		v.Text = fmt.Sprintf("At peak (%s) %s: another endpoint would help.", when, waits)
	case p95 >= verdictWaitNotable || share >= verdictShareNotable:
		v.Level = "noticeable"
		v.Text = fmt.Sprintf("At peak (%s) %s: noticeable, but one endpoint still keeps up.", when, waits)
	default:
		v.Level = "fine"
		v.Text = fmt.Sprintf("Even at the busiest time (%s) %s: one endpoint is enough.", when, waits)
	}
	return v
}

// slotClock is the time a slot starts, 24:00 for the end of the day.
func slotClock(slot int) string {
	m := slot * queueSlotMinutes
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

func seconds(v float64) string {
	switch {
	case v < 1:
		return strconv.Itoa(int(math.Round(v*1000))) + " ms"
	case v < 10:
		return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + " s"
	default:
		return strconv.Itoa(int(math.Round(v))) + " s"
	}
}
