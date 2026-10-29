// Package hygiene tells which dependencies are deprecated or behind their
// latest release (security roadmap item 8), from deps.dev: one request per
// package, carrying only the ecosystem and the package name. Go module
// versions are also checked for retractions on the Go module proxy
// (retract.go), carrying only module paths and versions. It runs only when
// deps.dev lookups are switched on, like licence lookups.
package hygiene

import (
	"context"
	"regexp"
	"sync"

	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/redact"
)

const (
	workers = 8
	// maxPackages bounds requests per ingest; the rest stay unchecked.
	maxPackages = 2000
)

// Status is what deps.dev says about one dependency version.
type Status struct {
	// Latest is the version the registry installs by default.
	Latest string
	// Deprecated is the publisher's reason, or "deprecated" without one;
	// empty when the version is not deprecated.
	Deprecated string
	// Behind counts stable releases published after this version.
	Behind int
}

// Check looks up deps, keyed by license.Dep.Key. Packages deps.dev does
// not know, failed requests and versions it does not list are left out.
func Check(ctx context.Context, c *license.DepsDev, deps []license.Dep) map[string]Status {
	byPackage := map[[2]string][]license.Dep{}
	var order [][2]string
	for _, d := range deps {
		k := [2]string{d.Ecosystem, d.Name}
		if _, ok := byPackage[k]; !ok {
			order = append(order, k)
		}
		byPackage[k] = append(byPackage[k], d)
	}
	var mu sync.Mutex
	out := map[string]Status{}
	each(ctx, order, func(k [2]string) {
		versions, err := c.Versions(ctx, k[0], k[1])
		if err != nil || len(versions) == 0 {
			return
		}
		for _, d := range byPackage[k] {
			if s, ok := StatusOf(versions, d.Version); ok {
				mu.Lock()
				out[d.Key()] = s
				mu.Unlock()
			}
		}
	})
	return out
}

// prerelease matches pre-release versions across ecosystems by their
// markers ("-rc.1", "-beta", "-SNAPSHOT", PyPI's "2.0b1" and "1.0.dev3")
// and Go pseudo-versions; a plain qualifier such as Maven's "-jre" is a
// release.
var prerelease = regexp.MustCompile(`(?i)(alpha|beta|rc|pre|preview|snapshot|dev|canary|next|nightly|milestone)|\d[ab]\d+$|-\d{14}-[0-9a-f]{12}$`)

// StatusOf is the status of current among versions, or false when current
// is not listed.
func StatusOf(versions []license.Version, current string) (Status, bool) {
	var cur *license.Version
	var s Status
	for i := range versions {
		v := &versions[i]
		if v.Version == current {
			cur = v
		}
		if v.Default {
			s.Latest = v.Version
		}
	}
	if cur == nil {
		return Status{}, false
	}
	if cur.Deprecated {
		s.Deprecated = "deprecated"
		if cur.Reason != "" {
			s.Deprecated = redact.Summary(cur.Reason, 200)
		}
	}
	if !cur.Published.IsZero() {
		for _, v := range versions {
			if v.Published.After(cur.Published) && !prerelease.MatchString(v.Version) {
				s.Behind++
			}
		}
	}
	return s, true
}
