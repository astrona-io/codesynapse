package usage

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"
)

// Feature timings (docs/plans/FEATURE_HEALTH_PLAN.md): every timed call adds
// to a fixed latency histogram, so buckets still merge by adding and the
// percentiles are read from the histogram; nothing is stored per call.

// LatencyBounds are the inclusive upper bounds, in milliseconds, of the
// histogram's buckets. A last bucket holds every slower call.
var LatencyBounds = [...]int{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 150000}

// LatencyBuckets is the histogram's length: the bounds plus the overflow.
const LatencyBuckets = len(LatencyBounds) + 1

// Latency counts timed calls per LatencyBounds bucket.
type Latency [LatencyBuckets]int

// Count is the number of timed calls.
func (h Latency) Count() int {
	n := 0
	for _, c := range h {
		n += c
	}
	return n
}

// latencyIndex is the bucket a call of ms milliseconds falls into.
func latencyIndex(ms int) int {
	i, _ := slices.BinarySearch(LatencyBounds[:], ms)
	return i
}

// Quantile estimates the q-quantile (0 < q <= 1) in milliseconds,
// interpolating inside the bucket that holds it and clamping to the calls'
// observed minimum and maximum. It is 0 without timed calls.
func (h Latency) Quantile(q float64, minMs, maxMs int) int {
	n := h.Count()
	if n == 0 {
		return 0
	}
	rank := math.Max(1, math.Ceil(q*float64(n)))
	seen := 0
	for i, c := range h {
		if c == 0 || float64(seen+c) < rank {
			seen += c
			continue
		}
		lo, hi := 0, maxMs
		if i > 0 {
			lo = LatencyBounds[i-1]
		}
		if i < len(LatencyBounds) {
			hi = LatencyBounds[i]
		}
		v := float64(lo) + (float64(hi-lo) * (rank - float64(seen)) / float64(c))
		return min(max(int(math.Round(v)), minMs), max(maxMs, minMs))
	}
	return maxMs
}

// LatencyColumns names one stored counter per histogram bucket: lat_<bound>
// and lat_over for the overflow.
func LatencyColumns() []string {
	out := make([]string, 0, LatencyBuckets)
	for _, b := range LatencyBounds {
		out = append(out, "lat_"+strconv.Itoa(b))
	}
	return append(out, "lat_over")
}

// Outcome is how a timed call ended.
type Outcome int

const (
	// OK is a call that answered.
	OK Outcome = iota
	// Failed is a refused or broken call; it counts as an error.
	Failed
	// TimedOut is a call that ran out of time; it counts as an error and a
	// timeout.
	TimedOut
)

// Outcomes are the names reports use for an outcome.
var Outcomes = map[string]Outcome{"ok": OK, "error": Failed, "timeout": TimedOut}

// ParseOutcome reads a reported outcome; "" is ok.
func ParseOutcome(s string) (Outcome, error) {
	if s == "" {
		return OK, nil
	}
	o, ok := Outcomes[s]
	if !ok {
		return OK, fmt.Errorf("unknown outcome %q", s)
	}
	return o, nil
}

// Observe counts one timed call of ms milliseconds in b.
func (b *Bucket) Observe(ms int, o Outcome) {
	ms = max(ms, 0)
	b.Add(Bucket{Calls: 1, DurationMs: ms, MinDurationMs: ms, MaxDurationMs: ms, Errors: btoi(o != OK), Timeouts: btoi(o == TimedOut),
		Latency: oneCall(ms)})
}

func oneCall(ms int) Latency {
	var h Latency
	h[latencyIndex(ms)] = 1
	return h
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// HourRetention is how long hourly buckets are kept: the Feature health
// windows reach back seven days.
const HourRetention = 8 * 24 * time.Hour

// Hourly returns b's copy for the hour of t: the Feature health page reads
// its 1 h, 24 h and 7 d windows from these. Day buckets keep the history.
func Hourly(b Bucket, t time.Time) Bucket {
	t = t.UTC()
	b.Day, b.Hour = Day(t), fmt.Sprintf("%02d", t.Hour())
	return b
}

// Spread returns b as the day bucket and its hourly copy, for a timed call
// added outside a Recorder.
func Spread(b Bucket, t time.Time) []Bucket {
	b.Day, b.Hour = Day(t), ""
	return []Bucket{b, Hourly(b, t)}
}

// Compare orders buckets by day, hour, organisation, repository, tool and
// client, the order the stores return them in.
func Compare(a, b Bucket) int {
	return cmp.Or(cmp.Compare(a.Day, b.Day), cmp.Compare(a.Hour, b.Hour), cmp.Compare(a.Org, b.Org), cmp.Compare(a.Repo, b.Repo),
		cmp.Compare(a.Tool, b.Tool), cmp.Compare(a.Client, b.Client))
}

// Start is the hour an hourly bucket covers (or the day of a day bucket).
func (b Bucket) Start() (time.Time, error) {
	if b.Hour == "" {
		return time.Parse(time.DateOnly, b.Day)
	}
	return time.Parse(time.DateOnly+"T15", b.Day+"T"+b.Hour)
}
