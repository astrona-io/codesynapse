package license

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
)

// Ecosystems, as named by OSV and vuln.Module.
const (
	Go   = "Go"
	NPM  = "npm"
	PyPI = "PyPI"
	// Lockfile-only ecosystems, as OSV names them.
	Cargo = "crates.io"
	NuGet = "NuGet"
	Maven = "Maven"
)

// Dep is one dependency to find a licence for.
type Dep struct {
	Ecosystem string
	Name      string
	Version   string
}

// Key identifies a dependency version.
func (d Dep) Key() string { return d.Ecosystem + "|" + d.Name + "|" + d.Version }

// Found is a licence and where it was read, for example
// "node_modules/react/package.json" or "deps.dev". The source is shown next
// to every verdict so a reviewer can check it.
type Found struct {
	License string `json:"license"`
	Source  string `json:"source"`
}

// Resolver finds dependency licences on disk and, when Online is set, from
// deps.dev. Only ecosystem, name and version leave the machine.
type Resolver struct {
	Root string
	// GoModCache is the Go module cache; "" uses GOMODCACHE or GOPATH/pkg/mod.
	GoModCache string
	Online     *DepsDev
}

// maxScanDirs bounds the directories searched for node_modules and
// virtualenvs.
const maxScanDirs = 5000

// Resolve returns licences for deps, keyed by Dep.Key. Dependencies it cannot
// resolve are left out.
func (r Resolver) Resolve(ctx context.Context, deps []Dep) map[string]Found {
	out := map[string]Found{}
	nodeDirs, sitePkgs := r.scan()
	py := pythonLicenses(sitePkgs)
	var missing []Dep
	for _, d := range deps {
		var f Found
		switch d.Ecosystem {
		case Go:
			f = r.goLicense(d)
		case NPM:
			f = r.npmLicense(nodeDirs, d)
		case PyPI:
			f = py[normalizePy(d.Name)]
		}
		if f.License != "" {
			out[d.Key()] = f
		} else {
			missing = append(missing, d)
		}
	}
	if r.Online != nil && len(missing) > 0 {
		for k, f := range r.Online.Lookup(ctx, missing) {
			out[k] = f
		}
	}
	return out
}

func (r Resolver) goLicense(d Dep) Found {
	if d.Name == "stdlib" {
		return Found{License: "BSD-3-Clause", Source: "Go standard library"}
	}
	vendor := filepath.Join(r.Root, "vendor", filepath.FromSlash(d.Name))
	if inside(r.Root, vendor) {
		if id, files := FromDir(vendor); id != "" {
			return Found{License: id, Source: "vendor/" + d.Name + "/" + files[0]}
		}
	}
	cache := r.GoModCache
	if cache == "" {
		cache = os.Getenv("GOMODCACHE")
	}
	if cache == "" {
		if gp := os.Getenv("GOPATH"); gp != "" {
			cache = filepath.Join(filepath.SplitList(gp)[0], "pkg", "mod")
		} else if home, err := os.UserHomeDir(); err == nil {
			cache = filepath.Join(home, "go", "pkg", "mod")
		}
	}
	p, err1 := module.EscapePath(d.Name)
	v, err2 := module.EscapeVersion(d.Version)
	if cache == "" || err1 != nil || err2 != nil {
		return Found{}
	}
	if id, files := FromDir(filepath.Join(cache, filepath.FromSlash(p)+"@"+v)); id != "" {
		return Found{License: id, Source: "Go module cache: " + d.Name + "@" + d.Version + "/" + files[0]}
	}
	return Found{}
}

// scan finds node_modules directories and Python site-packages under Root.
func (r Resolver) scan() (nodeDirs, sitePkgs []string) {
	n := 0
	_ = filepath.WalkDir(r.Root, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // best effort
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // unreadable entries are skipped
		}
		if n++; n > maxScanDirs {
			return filepath.SkipAll
		}
		name := d.Name()
		switch {
		case name == "node_modules":
			nodeDirs = append(nodeDirs, p)
			return filepath.SkipDir
		case name == "site-packages":
			sitePkgs = append(sitePkgs, p)
			return filepath.SkipDir
		case p != r.Root && (name == ".git" || name == "vendor" || name == "testdata"):
			return filepath.SkipDir
		case p != r.Root && strings.HasPrefix(name, ".") && name != ".venv":
			return filepath.SkipDir
		}
		return nil
	})
	return nodeDirs, sitePkgs
}

func (r Resolver) npmLicense(nodeDirs []string, d Dep) Found {
	for _, dir := range nodeDirs {
		p := filepath.Join(dir, filepath.FromSlash(d.Name), "package.json")
		if !inside(r.Root, p) {
			continue
		}
		b, err := readSmall(p)
		if err != nil {
			continue
		}
		var pkg struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(b, &pkg) != nil || (d.Version != "" && pkg.Version != d.Version) {
			continue
		}
		if id := NPMField(b); id != "" {
			rel, _ := filepath.Rel(r.Root, p)
			return Found{License: id, Source: filepath.ToSlash(rel)}
		}
	}
	return Found{}
}

// NPMField reads the licence from a package.json or package-lock entry: the
// SPDX "license" string, or the deprecated {"type": ...} and "licenses" list.
func NPMField(raw []byte) string {
	var pkg struct {
		License  json.RawMessage `json:"license"`
		Licenses []struct {
			Type string `json:"type"`
		} `json:"licenses"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(pkg.License, &s) == nil && s != "" {
		return Normalize(s)
	}
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(pkg.License, &obj) == nil && obj.Type != "" {
		return Normalize(obj.Type)
	}
	var types []string
	for _, l := range pkg.Licenses {
		if l.Type != "" {
			types = append(types, Normalize(l.Type))
		}
	}
	return strings.Join(types, " OR ")
}

var classifier = regexp.MustCompile(`^Classifier: License :: (?:OSI Approved :: )?(.+)$`)

// pythonLicenses reads installed packages' METADATA in site-packages.
func pythonLicenses(sitePkgs []string) map[string]Found {
	out := map[string]Found{}
	for _, sp := range sitePkgs {
		entries, err := os.ReadDir(sp)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasSuffix(e.Name(), ".dist-info") {
				continue
			}
			b, err := readSmall(filepath.Join(sp, e.Name(), "METADATA"))
			if err != nil {
				continue
			}
			name, id := parseMetadata(b)
			if name == "" || id == "" {
				continue
			}
			if _, ok := out[normalizePy(name)]; !ok {
				out[normalizePy(name)] = Found{License: id, Source: "installed package metadata (" + e.Name() + ")"}
			}
		}
	}
	return out
}

// parseMetadata returns the package name and licence from a core-metadata
// file: License-Expression first, then a short License field, then the
// trove classifiers.
func parseMetadata(b []byte) (name, id string) {
	var expr, field string
	var trove []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break // the body (long description) follows the headers
		}
		switch {
		case strings.HasPrefix(line, "Name: "):
			name = strings.TrimSpace(line[len("Name: "):])
		case strings.HasPrefix(line, "License-Expression: "):
			expr = strings.TrimSpace(line[len("License-Expression: "):])
		case strings.HasPrefix(line, "License: "):
			field = strings.TrimSpace(line[len("License: "):])
		default:
			if m := classifier.FindStringSubmatch(line); m != nil {
				trove = append(trove, Normalize(m[1]))
			}
		}
	}
	switch {
	case expr != "":
		return name, expr
	case field != "" && len(field) < 60 && Of(field) != Unknown:
		return name, Normalize(field)
	case len(trove) > 0:
		return name, strings.Join(trove, " OR ")
	}
	return name, ""
}

var pySeparators = regexp.MustCompile(`[-_.]+`)

// normalizePy applies PEP 503 name normalisation.
func normalizePy(name string) string {
	return pySeparators.ReplaceAllString(strings.ToLower(name), "-")
}

func readSmall(p string) ([]byte, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxLicenseFile {
		return nil, fs.ErrInvalid
	}
	return os.ReadFile(p) //nolint:gosec // callers check the path is inside the repository
}

// inside reports whether p resolves to a path within root.
func inside(root, p string) bool {
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rr, rp)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
