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

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
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
	Policy *repoconfig.Policy
	Root   string
	// GoModCache is the Go module cache; "" uses GOMODCACHE or GOPATH/pkg/mod.
	GoModCache string
	Online     *DepsDev
}

// Project resolves the repository licence through the same path policy as dependencies.
func (r Resolver) Project() Found { return Project(r.Root, r.allowed) }

// maxScanDirs bounds the directories searched for node_modules and
// virtualenvs.
const maxScanDirs = 5000

// Resolve returns licences for deps, keyed by Dep.Key. Dependencies it cannot
// resolve are left out.
func (r Resolver) Resolve(ctx context.Context, deps []Dep) map[string]Found {
	out := map[string]Found{}
	nodeDirs, sitePkgs := r.scan()
	py := pythonLicenses(sitePkgs, r.pythonReadable)
	var missing []Dep
	for _, d := range deps {
		var f Found
		switch d.Ecosystem {
		case Go:
			f = r.goLicense(d)
		case NPM:
			f = r.npmLicense(nodeDirs, d)
		case PyPI:
			f = pythonFor(py[normalizePy(d.Name)], d.Version)
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
	if inside(r.Root, vendor) && !r.Policy.PruneDir("vendor/"+d.Name) {
		if id, files := FromDir(vendor, r.allowed); id != "" {
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
		if allowed, policyErr := r.Policy.WalkEntry(r.Root, p, d); !allowed || policyErr != nil {
			return policyErr
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
		if !inside(r.Root, p) || !r.allowed(p) {
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

// installedPython is one installed distribution's licence evidence.
type installedPython struct {
	version string
	found   Found
}

// pythonLicenses reads installed packages' METADATA in site-packages. Every
// installed version is kept: several can be present, and a licence is
// evidence only for the version it was read from.
func pythonLicenses(sitePkgs []string, readable ...func(string) bool) map[string][]installedPython {
	out := map[string][]installedPython{}
	for _, sp := range sitePkgs {
		entries, err := os.ReadDir(sp)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasSuffix(e.Name(), ".dist-info") {
				continue
			}
			p := filepath.Join(sp, e.Name(), "METADATA")
			if len(readable) > 0 && !readable[0](p) {
				continue
			}
			b, err := readSmall(p)
			if err != nil {
				continue
			}
			name, version, id := parseMetadata(b)
			if name == "" || id == "" {
				continue
			}
			key := normalizePy(name)
			out[key] = append(out[key], installedPython{version: version,
				found: Found{License: id, Source: "installed package metadata (" + e.Name() + ")"}})
		}
	}
	return out
}

// pythonFor picks the evidence for the requested version. A mismatch stays
// unknown (the online lookup, when configured, answers per version); with no
// requested version only a single installed version is unambiguous.
func pythonFor(installed []installedPython, version string) Found {
	if version == "" {
		if len(installed) == 1 {
			return installed[0].found
		}
		return Found{}
	}
	for _, in := range installed {
		if in.version != "" && samePyVersion(in.version, version) {
			return in.found
		}
	}
	return Found{}
}

// samePyVersion compares two versions the way PEP 440 treats the common
// spellings: case, a leading "v" or "==", and trailing ".0" release segments
// do not matter ("1.0" and "1.0.0" are one version).
func samePyVersion(a, b string) bool { return canonicalPyVersion(a) == canonicalPyVersion(b) }

func canonicalPyVersion(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimPrefix(strings.TrimPrefix(v, "=="), "v")
	end := 0
	for end < len(v) && (v[end] == '.' || (v[end] >= '0' && v[end] <= '9')) {
		end++
	}
	release, rest := v[:end], v[end:]
	sep := ""
	if strings.HasSuffix(release, ".") { // "1.0.post1": the dot belongs to the suffix
		release, sep = strings.TrimSuffix(release, "."), "."
	}
	for strings.HasSuffix(release, ".0") {
		release = strings.TrimSuffix(release, ".0")
	}
	return release + sep + rest
}

// parseMetadata returns the package name and licence from a core-metadata
// file: License-Expression first, then a short License field, then the
// trove classifiers.
func parseMetadata(b []byte) (name, version, id string) {
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
		case strings.HasPrefix(line, "Version: "):
			version = strings.TrimSpace(line[len("Version: "):])
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
		return name, version, expr
	case field != "" && len(field) < 60 && Of(field) != Unknown:
		return name, version, Normalize(field)
	case len(trove) > 0:
		return name, version, strings.Join(trove, " OR ")
	}
	return name, version, ""
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

// pythonReadable says whether a METADATA file may be read: allowed by the
// policy and resolving to a path inside the repository, so a symlink in a
// virtualenv cannot pull in a file from outside the checkout.
func (r Resolver) pythonReadable(path string) bool {
	return r.allowed(path) && inside(r.Root, path)
}

func (r Resolver) allowed(path string) bool {
	rel, err := filepath.Rel(r.Root, path)
	return err == nil && r.Policy.PathAllowed(filepath.ToSlash(rel))
}
