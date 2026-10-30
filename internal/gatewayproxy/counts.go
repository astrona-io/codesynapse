package gatewayproxy

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"
)

// Limits of what is kept about outages: a few windows, a few model names
// each, counts that saturate. The file stays small however long the gateway
// is down.
const (
	MaxWindows      = 32
	MaxModels       = 8
	maxCount        = 100_000 // the gateway refuses more per window
	otherModel      = "other"
	maxModelNameLen = 64
	// maxAge is how old a window may be when it is reported; the gateway
	// takes 30 days, so older ones are dropped rather than sent.
	maxAge = 29 * 24 * time.Hour
)

// modelName is the shape of a model name worth keeping; anything else
// (client text that is not a plain name) is counted as "other". It matches
// the gateway's own rule (services/api/internal/api/gateway.go).
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$`)

// CleanModel turns a requested model into a name that may be kept and sent.
func CleanModel(m string) string {
	if len(m) > maxModelNameLen || !modelName.MatchString(m) {
		return otherModel
	}
	return m
}

// Window is one outage as the proxy saw it: when it began and ended, how many
// requests went direct, and by which model. Nothing else is kept.
type Window struct {
	Start    time.Time      `json:"start"`
	End      time.Time      `json:"end"`
	Requests int            `json:"requests"`
	Models   map[string]int `json:"models,omitempty"`
	// Open is true while the outage lasts; only closed windows are reported.
	Open bool `json:"open,omitempty"`
}

// counts is the persistent list of outage windows (gateway-failover.json).
type counts struct {
	mu   sync.Mutex
	path string
	w    []Window
}

func loadWindows(p string) ([]Window, error) {
	b, err := readSmall(p)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Windows []Window `json:"windows"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc.Windows, nil
}

// openCounts reads what an earlier proxy left. A window that was still open
// when it stopped is closed at its last request: that proxy can no longer
// say when the outage ended.
func openCounts(p string) *counts {
	c := &counts{path: p}
	w, err := loadWindows(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		w = nil // unreadable: start over rather than fail the proxy
	}
	for i := range w {
		w[i].Open = false
		if w[i].End.Before(w[i].Start) {
			w[i].End = w[i].Start
		}
	}
	if len(w) > MaxWindows {
		w = w[len(w)-MaxWindows:]
	}
	c.w = w
	return c
}

func (c *counts) saveLocked() {
	b, err := json.Marshal(struct {
		Windows []Window `json:"windows"`
	}{c.w})
	if err == nil {
		_ = writePrivate(c.path, b) //nolint:errcheck // counts are best effort; the proxy keeps working
	}
}

func (c *counts) current() *Window {
	if n := len(c.w); n > 0 && c.w[n-1].Open {
		return &c.w[n-1]
	}
	return nil
}

// open starts an outage window unless one is open.
func (c *counts) open(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current() == nil {
		c.openLocked(now)
		c.saveLocked()
	}
}

func (c *counts) openLocked(now time.Time) {
	if len(c.w) >= MaxWindows {
		// Fold the two oldest together so the count of requests survives.
		a, b := c.w[0], c.w[1]
		a.End, a.Requests = b.End, min(a.Requests+b.Requests, maxCount)
		a.Models = mergeModels(a.Models, b.Models)
		c.w = append([]Window{a}, c.w[2:]...)
	}
	c.w = append(c.w, Window{Start: now, End: now, Open: true})
}

// direct counts one request that went straight to Anthropic. It goes into
// the open window; when the outage already ended (the probe found the
// gateway while this request was under way), into a closed window of its
// own, so no window is left open while the gateway is up.
func (c *counts) direct(now time.Time, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.current()
	if w == nil {
		c.openLocked(now)
		w = c.current()
		w.Open = false
	}
	w.End = now
	w.Requests = min(w.Requests+1, maxCount)
	w.Models = mergeModels(w.Models, map[string]int{CleanModel(model): 1})
	c.saveLocked()
}

// close ends the open window.
func (c *counts) close(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w := c.current(); w != nil {
		w.End, w.Open = now, false
		c.saveLocked()
	}
}

// pending returns copies of the closed windows, oldest first, and drops
// those too old for the gateway to take. The copies share nothing with the
// list, which keeps changing while a report is under way.
func (c *counts) pending(now time.Time) []Window {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.w)
	c.w = slices.DeleteFunc(c.w, func(w Window) bool { return !w.Open && w.Start.Before(now.Add(-maxAge)) })
	if len(c.w) != n {
		c.saveLocked()
	}
	var out []Window
	for _, w := range c.w {
		if !w.Open {
			w.Models = maps.Clone(w.Models)
			out = append(out, w)
		}
	}
	return out
}

// drop removes the reported windows (matched by start time).
func (c *counts) drop(sent []Window) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w = slices.DeleteFunc(c.w, func(w Window) bool {
		return !w.Open && slices.ContainsFunc(sent, func(s Window) bool { return s.Start.Equal(w.Start) })
	})
	c.saveLocked()
}

// mergeModels adds b to a, keeping at most MaxModels names: the rest count
// as "other".
func mergeModels(a, b map[string]int) map[string]int {
	if a == nil {
		a = map[string]int{}
	}
	names := make([]string, 0, len(b))
	for k := range b {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, name := range names {
		named := len(a)
		if _, ok := a[otherModel]; ok {
			named--
		}
		k := name
		if _, ok := a[k]; !ok && k != otherModel && named >= MaxModels-1 {
			k = otherModel
		}
		a[k] = min(a[k]+b[name], maxCount)
	}
	return a
}
