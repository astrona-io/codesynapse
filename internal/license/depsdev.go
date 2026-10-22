package license

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DepsDev looks up licences on deps.dev. The zero value uses
// https://api.deps.dev. Each request carries one ecosystem, package name and
// version, nothing else.
type DepsDev struct {
	BaseURL string
	HTTP    *http.Client
}

const (
	depsDevURL     = "https://api.deps.dev"
	depsDevWorkers = 8
	// maxLookups bounds requests per ingest; huge lockfiles keep the rest
	// unknown rather than hammering the service.
	maxLookups  = 2000
	maxResponse = 1 << 20
)

var systems = map[string]string{Go: "GO", NPM: "NPM", PyPI: "PYPI"}

// Lookup returns licences for deps, keyed by Dep.Key. Failed lookups are left
// out; the caller treats them as unknown.
func (c *DepsDev) Lookup(ctx context.Context, deps []Dep) map[string]Found {
	if len(deps) > maxLookups {
		deps = deps[:maxLookups]
	}
	var mu sync.Mutex
	out := map[string]Found{}
	work := make(chan Dep)
	var wg sync.WaitGroup
	for range depsDevWorkers {
		wg.Go(func() {
			for d := range work {
				if id, err := c.one(ctx, d); err == nil && id != "" {
					mu.Lock()
					out[d.Key()] = Found{License: id, Source: "deps.dev"}
					mu.Unlock()
				}
			}
		})
	}
	for _, d := range deps {
		if ctx.Err() != nil {
			break
		}
		work <- d
	}
	close(work)
	wg.Wait()
	return out
}

// escape encodes a name as one path segment; deps.dev expects "@" as %40
// too ("@scope/pkg" -> "%40scope%2Fpkg"), which url.PathEscape keeps.
func escape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "@", "%40")
}

func (c *DepsDev) one(ctx context.Context, d Dep) (string, error) {
	sys, ok := systems[d.Ecosystem]
	if !ok || d.Name == "" || d.Version == "" {
		return "", nil
	}
	base := depsDevURL
	if c.BaseURL != "" {
		base = strings.TrimRight(c.BaseURL, "/")
	}
	u := fmt.Sprintf("%s/v3/systems/%s/packages/%s/versions/%s", base, sys, escape(d.Name), escape(d.Version))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "codesynapse")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("deps.dev: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck // read-only response
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deps.dev: status %d", res.StatusCode)
	}
	var v struct {
		Licenses []string `json:"licenses"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxResponse)).Decode(&v); err != nil {
		return "", err
	}
	// Several entries are several licence files; all of them apply.
	var parts []string
	for _, l := range v.Licenses {
		if l = strings.TrimSpace(l); l != "" {
			if strings.Contains(l, " ") {
				l = "(" + l + ")"
			}
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " AND "), nil
}
