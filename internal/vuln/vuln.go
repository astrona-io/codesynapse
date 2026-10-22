// Package vuln finds known vulnerabilities in a repository's Go dependencies
// and the Go standard library, using the OSV database (osv.dev, which
// mirrors the Go vulnerability database and GitHub advisories).
//
// Each vulnerability gets a reachability level, the same idea as govulncheck:
//
//	called    repository code calls a vulnerable symbol
//	imported  repository code imports an affected package but no vulnerable symbol is called
//	required  the module is only in go.mod (possibly needed by another dependency)
//
// Only module paths and versions are sent to osv.dev, never source code.
package vuln

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// Module is a required module version.
type Module struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Indirect bool   `json:"indirect,omitempty"`
	// Ecosystem is Go (default), npm or PyPI.
	Ecosystem string `json:"ecosystem,omitempty"`
	// Dev marks development-only dependencies (npm dev, Pipfile develop).
	Dev bool `json:"dev,omitempty"`
	// License and LicenseSource are filled from the lockfile when it records
	// them, otherwise by package license during ingest.
	License       string `json:"license,omitempty"`
	LicenseSource string `json:"license_source,omitempty"`
}

func (m Module) ecosystem() string {
	if m.Ecosystem == "" {
		return EcosystemGo
	}
	return m.Ecosystem
}

// Key identifies a module across ecosystems; Go keeps the bare path.
func (m Module) Key() string {
	if m.ecosystem() == EcosystemGo {
		return m.Path
	}
	return m.ecosystem() + ":" + m.Path
}

// StdlibPath is the module path OSV uses for the Go standard library.
const StdlibPath = "stdlib"

// Modules reads every go.mod under root (skipping vendor, testdata, hidden
// directories and nested repositories) and returns the union of
// requirements plus the standard library. In-repo modules are excluded.
//
// The standard library is checked at the Go version that builds the code:
// buildGo (the local toolchain, e.g. runtime.Version()) or a newer
// `toolchain` line. The `go` directive is only a minimum, so using it would
// report years of fixed CVEs for any library that supports old releases.
func Modules(root, buildGo string) ([]Module, error) {
	byPath := map[string]Module{}
	own := map[string]bool{}
	stdlib := ""
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "vendor" || name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") || isRepo(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // path is inside root and not a symlink
		if err != nil {
			return err
		}
		// Parse (not ParseLax) so toolchain directives are read; fall back to
		// the lax parser for go.mod files with newer syntax than x/mod knows.
		mf, err := modfile.Parse(path, data, nil)
		if err != nil {
			if mf, err = modfile.ParseLax(path, data, nil); err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
		}
		if mf.Module != nil {
			own[mf.Module.Mod.Path] = true
		}
		if mf.Toolchain != nil {
			if v := goVersion(mf); v != "" && (stdlib == "" || semver.Compare(v, stdlib) > 0) {
				stdlib = v
			}
		}
		for _, r := range mf.Require {
			prev, ok := byPath[r.Mod.Path]
			if !ok || semver.Compare(r.Mod.Version, prev.Version) > 0 {
				byPath[r.Mod.Path] = Module{Path: r.Mod.Path, Version: r.Mod.Version, Indirect: r.Indirect && (!ok || prev.Indirect)}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]Module, 0, len(byPath)+1)
	for p, m := range byPath {
		if !own[p] {
			out = append(out, m)
		}
	}
	if b := semverGo(buildGo); b != "" && (stdlib == "" || semver.Compare(b, stdlib) > 0) {
		stdlib = b
	}
	if stdlib != "" {
		out = append(out, Module{Path: StdlibPath, Version: stdlib})
	}
	slices.SortFunc(out, func(a, b Module) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}

func isRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// semverGo turns "go1.27.3" (runtime.Version) or "1.27" into "v1.27.3"/"v1.27.0".
func semverGo(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "go")
	if i := strings.IndexAny(v, " -"); i >= 0 { // "1.27.3 X:..." or devel builds
		v = v[:i]
	}
	if v == "" || strings.HasPrefix(v, "devel") {
		return ""
	}
	if strings.Count(v, ".") == 1 {
		v += ".0"
	}
	if !semver.IsValid("v" + v) {
		return ""
	}
	return "v" + v
}

// goVersion returns the toolchain version (or the go directive) as semver.
func goVersion(mf *modfile.File) string {
	v := ""
	if mf.Toolchain != nil {
		v = strings.TrimPrefix(mf.Toolchain.Name, "go")
	} else if mf.Go != nil {
		v = mf.Go.Version
	}
	if v == "" {
		return ""
	}
	if strings.Count(v, ".") == 1 {
		v += ".0"
	}
	return "v" + v
}

// Vulnerability is an advisory affecting one module version.
type Vulnerability struct {
	ID        string   `json:"id"`
	Aliases   []string `json:"aliases,omitempty"`
	Summary   string   `json:"summary"`
	Severity  string   `json:"severity"`
	CWE       []string `json:"cwe,omitempty"`
	Module    string   `json:"module"`
	Ecosystem string   `json:"ecosystem,omitempty"`
	Version   string   `json:"version"`
	Fixed     string   `json:"fixed,omitempty"`
	URL       string   `json:"url"`
	// Symbols maps an affected import path to its vulnerable symbols
	// ("Get", "Client.Do"); an empty list means the whole package.
	Symbols map[string][]string `json:"symbols,omitempty"`
}

// Client queries the OSV API. The zero value uses https://api.osv.dev.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	mu    sync.Mutex
	cache map[string]osvEntry
}

const (
	defaultBaseURL = "https://api.osv.dev"
	maxBatch       = 1000
	maxResponse    = 16 << 20
	detailWorkers  = 8
)

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultBaseURL
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

type osvEntry struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary"`
	Details  string   `json:"details"`
	Aliases  []string `json:"aliases"`
	Severity []struct {
		Type  string `json:"type"`
		Score string `json:"score"`
	} `json:"severity"`
	Affected []struct {
		Package struct {
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
		} `json:"package"`
		Ranges []struct {
			Type   string `json:"type"`
			Events []struct {
				Introduced string `json:"introduced"`
				Fixed      string `json:"fixed"`
			} `json:"events"`
		} `json:"ranges"`
		EcosystemSpecific struct {
			Imports []struct {
				Path    string   `json:"path"`
				Symbols []string `json:"symbols"`
			} `json:"imports"`
		} `json:"ecosystem_specific"`
	} `json:"affected"`
	DatabaseSpecific struct {
		Severity string   `json:"severity"`
		CWEIDs   []string `json:"cwe_ids"`
		URL      string   `json:"url"`
	} `json:"database_specific"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codesynapse")
	res, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("osv %s: %w", path, err)
	}
	defer res.Body.Close() //nolint:errcheck // read-only response
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("osv %s: status %d", path, res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, maxResponse)).Decode(out)
}

// Scan returns the vulnerabilities affecting modules.
func (c *Client) Scan(ctx context.Context, modules []Module) ([]Vulnerability, error) {
	type query struct {
		Package struct {
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
		} `json:"package"`
		Version string `json:"version"`
	}
	type pending struct {
		id  string
		mod Module
	}
	var todo []pending
	for start := 0; start < len(modules); start += maxBatch {
		batch := modules[start:min(start+maxBatch, len(modules))]
		qs := make([]query, len(batch))
		for i, m := range batch {
			qs[i].Package.Name, qs[i].Package.Ecosystem, qs[i].Version = m.Path, m.ecosystem(), m.Version
		}
		var res struct {
			Results []struct {
				Vulns []struct {
					ID string `json:"id"`
				} `json:"vulns"`
			} `json:"results"`
		}
		if err := c.do(ctx, http.MethodPost, "/v1/querybatch", map[string]any{"queries": qs}, &res); err != nil {
			return nil, err
		}
		for i, r := range res.Results {
			if i >= len(batch) {
				break
			}
			// Prefer GO- entries: they carry symbol data. GHSA duplicates are
			// folded in through aliases below.
			for _, v := range r.Vulns {
				todo = append(todo, pending{id: v.ID, mod: batch[i]})
			}
		}
	}

	results := make([]Vulnerability, len(todo))
	errs := make([]error, len(todo))
	sem := make(chan struct{}, detailWorkers)
	var wg sync.WaitGroup
	for i, p := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			results[i], errs[i] = c.detail(ctx, p.id, p.mod)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return dedupe(results), nil
}

func (c *Client) entry(ctx context.Context, id string) (osvEntry, error) {
	c.mu.Lock()
	if e, ok := c.cache[id]; ok {
		c.mu.Unlock()
		return e, nil
	}
	c.mu.Unlock()
	var e osvEntry
	if err := c.do(ctx, http.MethodGet, "/v1/vulns/"+id, nil, &e); err != nil {
		return e, err
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]osvEntry{}
	}
	c.cache[id] = e
	c.mu.Unlock()
	return e, nil
}

func (c *Client) detail(ctx context.Context, id string, mod Module) (Vulnerability, error) {
	e, err := c.entry(ctx, id)
	if err != nil {
		return Vulnerability{}, err
	}
	v := Vulnerability{
		ID: e.ID, Aliases: e.Aliases, Summary: e.Summary, Module: mod.Path, Ecosystem: mod.ecosystem(), Version: mod.Version,
		Severity: normaliseSeverity(e.DatabaseSpecific.Severity), CWE: e.DatabaseSpecific.CWEIDs,
		URL: advisoryURL(e), Symbols: map[string][]string{},
	}
	if v.Summary == "" {
		v.Summary = firstLine(e.Details)
	}
	for _, a := range e.Affected {
		if a.Package.Name != mod.Path && (mod.ecosystem() != EcosystemPyPI || normalizePy(a.Package.Name) != mod.Path) {
			continue
		}
		v.Fixed = fixedFor(mod.Version, a.Ranges)
		for _, imp := range a.EcosystemSpecific.Imports {
			v.Symbols[imp.Path] = imp.Symbols
		}
	}
	// Go vulndb entries carry no severity; borrow it (and CWEs) from the
	// GitHub advisory alias.
	if v.Severity == "" || len(v.CWE) == 0 {
		for _, alias := range e.Aliases {
			if !strings.HasPrefix(alias, "GHSA-") {
				continue
			}
			ghsa, err := c.entry(ctx, alias)
			if err != nil {
				break // severity is enrichment; keep the finding
			}
			if v.Severity == "" {
				v.Severity = normaliseSeverity(ghsa.DatabaseSpecific.Severity)
			}
			if len(v.CWE) == 0 {
				v.CWE = ghsa.DatabaseSpecific.CWEIDs
			}
			break
		}
	}
	if v.Severity == "" {
		v.Severity = "medium" // unknown: assume medium rather than hide it
	}
	return v, nil
}

func advisoryURL(e osvEntry) string {
	if e.DatabaseSpecific.URL != "" {
		return e.DatabaseSpecific.URL
	}
	return "https://osv.dev/vulnerability/" + e.ID
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func normaliseSeverity(s string) string {
	switch strings.ToUpper(s) {
	case "LOW":
		return "low"
	case "MODERATE", "MEDIUM":
		return "medium"
	case "HIGH":
		return "high"
	case "CRITICAL":
		return "critical"
	}
	return ""
}

// fixedFor returns the lowest fixed version above current within the range
// that contains it.
func fixedFor(current string, ranges []struct {
	Type   string `json:"type"`
	Events []struct {
		Introduced string `json:"introduced"`
		Fixed      string `json:"fixed"`
	} `json:"events"`
}) string {
	best := ""
	cur := "v" + strings.TrimPrefix(current, "v")
	for _, r := range ranges {
		for _, ev := range r.Events {
			if ev.Fixed == "" {
				continue
			}
			f := "v" + strings.TrimPrefix(ev.Fixed, "v")
			if !semver.IsValid(f) || !semver.IsValid(cur) {
				// PEP 440 and other schemes: report the first fixed version as-is.
				if best == "" {
					best = ev.Fixed
				}
				continue
			}
			if semver.Compare(f, cur) > 0 && (best == "" || semver.Compare(f, best) < 0) {
				best = f
			}
		}
	}
	if strings.HasPrefix(current, "v") || best == "" {
		return best
	}
	return strings.TrimPrefix(best, "v")
}

// dedupe drops GHSA entries that duplicate a GO- entry for the same module.
func dedupe(vs []Vulnerability) []Vulnerability {
	covered := map[string]bool{}
	for _, v := range vs {
		// Prefer ecosystem advisories (GO-, PYSEC-) over their GHSA copies.
		if strings.HasPrefix(v.ID, "GO-") || strings.HasPrefix(v.ID, "PYSEC-") {
			for _, a := range v.Aliases {
				covered[v.Module+"|"+a] = true
			}
		}
	}
	seen := map[string]bool{}
	var out []Vulnerability
	for _, v := range vs {
		key := v.Module + "|" + v.ID
		if seen[key] || covered[key] {
			continue
		}
		seen[key] = true
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b Vulnerability) int { return cmp.Or(cmp.Compare(a.Module, b.Module), cmp.Compare(a.ID, b.ID)) })
	return out
}
