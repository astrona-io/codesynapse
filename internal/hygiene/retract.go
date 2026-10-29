package hygiene

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// deps.dev does not expose Go retractions, so they come from the Go module
// proxy: the retract directives of the go.mod of a module's latest version.

const (
	goProxyURL = "https://proxy.golang.org"
	// maxResponse caps one proxy answer, like the deps.dev client's.
	maxResponse = 1 << 20
	// maxRationale caps the stored retraction rationale, like a
	// deprecation reason.
	maxRationale = 200
)

// GoProxy reads go.mod files from a Go module proxy. The zero value uses
// https://proxy.golang.org. Each request carries one module path and, for
// the go.mod, its latest version, nothing else.
type GoProxy struct {
	BaseURL string
	HTTP    *http.Client
}

var errRedirect = errors.New("redirect refused")

// noRedirect keeps every request on the configured proxy.
func noRedirect(*http.Request, []*http.Request) error { return errRedirect }

// get fetches one proxy path, capped at maxResponse.
func (p *GoProxy) get(ctx context.Context, path string) ([]byte, error) {
	base := goProxyURL
	if p.BaseURL != "" {
		base = strings.TrimRight(p.BaseURL, "/")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "codesynapse")
	hc := &http.Client{Timeout: 15 * time.Second}
	if p.HTTP != nil {
		c := *p.HTTP
		hc = &c
	}
	hc.CheckRedirect = noRedirect
	start := time.Now()
	res, err := hc.Do(req)
	telemetry.RecordHTTPLookup(ctx, telemetry.LookupGoProxy, start, res, err)
	if err != nil {
		return nil, fmt.Errorf("go proxy: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck // read-only response
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("go proxy: status %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, maxResponse))
}

// Retractions returns the retract directives of the go.mod of the latest
// version of the module at path, as the proxy's @latest names it.
func (p *GoProxy) Retractions(ctx context.Context, path string) ([]*modfile.Retract, error) {
	esc, err := module.EscapePath(path)
	if err != nil {
		return nil, err
	}
	body, err := p.get(ctx, "/"+esc+"/@latest")
	if err != nil {
		return nil, err
	}
	var info struct{ Version string }
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("go proxy: @latest: %w", err)
	}
	if err := module.Check(path, info.Version); err != nil {
		return nil, fmt.Errorf("go proxy: @latest: %w", err)
	}
	escVer, err := module.EscapeVersion(info.Version)
	if err != nil {
		return nil, err
	}
	body, err = p.get(ctx, "/"+esc+"/@v/"+escVer+".mod")
	if err != nil {
		return nil, err
	}
	f, err := modfile.ParseLax("go.mod", body, nil)
	if err != nil {
		return nil, fmt.Errorf("go proxy: go.mod: %w", err)
	}
	return f.Retract, nil
}

// RetractedBy is the rationale of the first directive in rs that covers
// version ("retracted" when it gives none), or false when none does.
func RetractedBy(rs []*modfile.Retract, version string) (string, bool) {
	if !semver.IsValid(version) {
		return "", false
	}
	for _, r := range rs {
		if semver.Compare(r.Low, version) <= 0 && semver.Compare(version, r.High) <= 0 {
			if r.Rationale == "" {
				return "retracted", true
			}
			return redact.Summary(r.Rationale, maxRationale), true
		}
	}
	return "", false
}

// Retracted looks up which Go module versions among deps are retracted,
// keyed by license.Dep.Key, with the rationale as the value. Other
// ecosystems, invalid module paths and failed requests are left out.
func Retracted(ctx context.Context, p *GoProxy, deps []license.Dep) map[string]string {
	byModule := map[string][]license.Dep{}
	var order []string
	for _, d := range deps {
		if d.Ecosystem != license.Go || module.CheckPath(d.Name) != nil {
			continue
		}
		if _, ok := byModule[d.Name]; !ok {
			order = append(order, d.Name)
		}
		byModule[d.Name] = append(byModule[d.Name], d)
	}
	var mu sync.Mutex
	out := map[string]string{}
	each(ctx, order, func(path string) {
		rs, err := p.Retractions(ctx, path)
		if err != nil || len(rs) == 0 {
			return
		}
		for _, d := range byModule[path] {
			if why, ok := RetractedBy(rs, d.Version); ok {
				mu.Lock()
				out[d.Key()] = why
				mu.Unlock()
			}
		}
	})
	return out
}

// each runs fn over the first maxPackages keys with a fixed number of
// workers, stopping early when ctx ends.
func each[K any](ctx context.Context, keys []K, fn func(K)) {
	keys = keys[:min(len(keys), maxPackages)]
	work := make(chan K)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for k := range work {
				fn(k)
			}
		})
	}
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		work <- k
	}
	close(work)
	wg.Wait()
}
