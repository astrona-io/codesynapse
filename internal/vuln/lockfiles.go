package vuln

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Lockfiles of further ecosystems. Each parser reads only what the lockfile
// resolved (name, version, who requires whom) plus the manifest next to it for
// which dependencies the project declares itself. Source files of these
// languages are not indexed, so reachability for them is always "required".

func key(eco, name string) string { return (Module{Path: name, Ecosystem: eco}).Key() }

func sortedUnique(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
}

// --- Rust: Cargo.lock ---------------------------------------------------------

var (
	cargoName    = regexp.MustCompile(`(?m)^name\s*=\s*"([^"]+)"`)
	cargoVersion = regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`)
	cargoSource  = regexp.MustCompile(`(?m)^source\s*=\s*"([^"]+)"`)
	cargoDeps    = regexp.MustCompile(`(?s)\ndependencies = \[(.*?)\n\]`)
	quoted       = regexp.MustCompile(`"([^"]+)"`)
)

// parseCargoLock reads [[package]] blocks. Packages without a source are the
// workspace's own crates. dir holds Cargo.toml (and, for a workspace, its
// members below it), which says which crates the project declares.
func parseCargoLock(data []byte, dir string) []Module {
	runtime, dev := cargoDeclared(dir)
	var out []Module
	for _, block := range tomlPackage.Split(string(data), -1)[1:] {
		n, v, src := cargoName.FindStringSubmatch(block), cargoVersion.FindStringSubmatch(block), cargoSource.FindStringSubmatch(block)
		if n == nil || v == nil || src == nil {
			continue
		}
		var requires []string
		if m := cargoDeps.FindStringSubmatch(block); m != nil {
			for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
				name, _, _ := strings.Cut(q[1], " ")
				requires = append(requires, key(EcosystemCargo, name))
			}
		}
		name := n[1]
		declared := runtime[name] || dev[name]
		out = append(out, Module{Path: name, Version: v[1], Ecosystem: EcosystemCargo,
			Indirect: !declared && (len(runtime)+len(dev)) > 0, Dev: !runtime[name] && dev[name], Requires: sortedUnique(requires)})
	}
	return out
}

// cargoDeclared returns the crates named in [dependencies] and
// [build-dependencies] (runtime) and [dev-dependencies] (dev) of the Cargo.toml
// files at dir and one to three levels below it.
func cargoDeclared(dir string) (runtime, dev map[string]bool) {
	runtime, dev = map[string]bool{}, map[string]bool{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error { //nolint:errcheck // best effort
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are skipped
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(dir, p) //nolint:errcheck // p is below dir
			if n := d.Name(); p != dir && (n == "target" || n == "vendor" || n == "node_modules" || strings.HasPrefix(n, ".")) || strings.Count(rel, string(filepath.Separator)) > 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "Cargo.toml" || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil { //nolint:gosec // inside the scanned root, not a symlink
			cargoToml(string(b), runtime, dev)
		}
		return nil
	})
	return runtime, dev
}

var cargoSection = regexp.MustCompile(`^\[(?:target\.[^\]]+\.)?(workspace\.)?(dependencies|dev-dependencies|build-dependencies)(?:\.([A-Za-z0-9_.-]+))?\]$`)

func cargoToml(text string, runtime, dev map[string]bool) {
	var into map[string]bool
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "["):
			into = nil
			if m := cargoSection.FindStringSubmatch(t); m != nil {
				into = runtime
				if m[2] == "dev-dependencies" {
					into = dev
				}
				if m[3] != "" { // [dependencies.serde]
					into[m[3]] = true
					into = nil
				}
			}
		case into != nil && t != "" && !strings.HasPrefix(t, "#"):
			if name, _, ok := strings.Cut(t, "="); ok {
				into[strings.Trim(strings.TrimSpace(strings.SplitN(name, ".", 2)[0]), `"`)] = true
			}
		}
	}
}

// --- PHP: composer.lock -------------------------------------------------------

func parseComposerLock(data []byte, dir string) []Module {
	var lock struct {
		Packages    []composerPackage `json:"packages"`
		PackagesDev []composerPackage `json:"packages-dev"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return nil
	}
	direct, haveManifest := composerDirect(dir)
	var out []Module
	for _, set := range []struct {
		pkgs []composerPackage
		dev  bool
	}{{lock.Packages, false}, {lock.PackagesDev, true}} {
		for _, p := range set.pkgs {
			if p.Name == "" || p.Version == "" {
				continue
			}
			var requires []string
			for name := range p.Require {
				if name == "php" || strings.HasPrefix(name, "ext-") || strings.HasPrefix(name, "lib-") {
					continue
				}
				requires = append(requires, key(EcosystemPackagist, name))
			}
			m := Module{Path: p.Name, Version: strings.TrimPrefix(p.Version, "v"), Ecosystem: EcosystemPackagist, Dev: set.dev,
				Indirect: haveManifest && !direct[p.Name], Requires: sortedUnique(requires)}
			if len(p.License) > 0 {
				m.License, m.LicenseSource = strings.Join(p.License, " OR "), "composer.lock"
			}
			out = append(out, m)
		}
	}
	return out
}

type composerPackage struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	License []string          `json:"license"`
	Require map[string]string `json:"require"`
}

func composerDirect(dir string) (map[string]bool, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "composer.json")) //nolint:gosec // next to a lockfile inside root
	if err != nil {
		return nil, false
	}
	var c struct {
		Require    map[string]string `json:"require"`
		RequireDev map[string]string `json:"require-dev"`
	}
	if json.Unmarshal(data, &c) != nil {
		return nil, false
	}
	direct := map[string]bool{}
	for n := range c.Require {
		direct[n] = true
	}
	for n := range c.RequireDev {
		direct[n] = true
	}
	return direct, true
}

// --- Ruby: Gemfile.lock -------------------------------------------------------

var (
	gemSpec    = regexp.MustCompile(`^    ([A-Za-z0-9_.-]+) \(([^)]+)\)$`)
	gemRequire = regexp.MustCompile(`^      ([A-Za-z0-9_.-]+)(?: \(.*\))?$`)
	gemDirect  = regexp.MustCompile(`^  ([A-Za-z0-9_.-]+)(?: \(.*\))?!?$`)
	gemVersion = regexp.MustCompile(`^[0-9][^-]*`)
)

// parseGemfileLock reads the GEM specs (resolved gems, with the gems each
// requires one level deeper) and the DEPENDENCIES the Gemfile declares.
func parseGemfileLock(data []byte, _ string) []Module {
	var out []Module
	direct := map[string]bool{}
	section, inSpecs := "", false
	cur := -1
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \r")
		switch {
		case line == "":
			continue
		case !strings.HasPrefix(line, " "):
			section, inSpecs, cur = line, false, -1
		case section == "GEM" && strings.TrimSpace(line) == "specs:":
			inSpecs = true
		case section == "DEPENDENCIES":
			if m := gemDirect.FindStringSubmatch(line); m != nil {
				direct[m[1]] = true
			}
		case section == "GEM" && inSpecs:
			if m := gemSpec.FindStringSubmatch(line); m != nil {
				// "1.2.3-x86_64-linux": the platform is not part of the version.
				v := gemVersion.FindString(m[2])
				out = append(out, Module{Path: m[1], Version: v, Ecosystem: EcosystemRubyGems})
				cur = len(out) - 1
			} else if m := gemRequire.FindStringSubmatch(line); m != nil && cur >= 0 {
				out[cur].Requires = append(out[cur].Requires, key(EcosystemRubyGems, m[1]))
			}
		}
	}
	if len(direct) > 0 {
		for i := range out {
			out[i].Indirect = !direct[out[i].Path]
			out[i].Requires = sortedUnique(out[i].Requires)
		}
	}
	return out
}

// --- .NET: packages.lock.json -------------------------------------------------

func parseNuGetLock(data []byte, _ string) []Module {
	var lock struct {
		Dependencies map[string]map[string]struct {
			Type         string            `json:"type"`
			Resolved     string            `json:"resolved"`
			Dependencies map[string]string `json:"dependencies"`
		} `json:"dependencies"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return nil
	}
	byName := map[string]*Module{}
	var order []string
	for _, framework := range lock.Dependencies {
		for name, p := range framework {
			if p.Resolved == "" || p.Type == "Project" {
				continue
			}
			m := byName[name]
			if m == nil {
				m = &Module{Path: name, Version: p.Resolved, Ecosystem: EcosystemNuGet, Indirect: true}
				byName[name] = m
				order = append(order, name)
			}
			m.Indirect = m.Indirect && p.Type != "Direct"
			for dep := range p.Dependencies {
				m.Requires = append(m.Requires, key(EcosystemNuGet, dep))
			}
		}
	}
	slices.Sort(order)
	out := make([]Module, 0, len(order))
	for _, n := range order {
		byName[n].Requires = sortedUnique(byName[n].Requires)
		out = append(out, *byName[n])
	}
	return out
}

// --- Java and Kotlin: gradle.lockfile -----------------------------------------

// parseGradleLock reads "group:artifact:version=configuration,..." lines.
// The build file next to the lockfile says which coordinates the project names
// itself; the lockfile has no dependency edges.
func parseGradleLock(data []byte, dir string) []Module {
	build := ""
	for _, f := range []string{"build.gradle.kts", "build.gradle"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil { //nolint:gosec // next to a lockfile inside root
			build += string(b)
		}
	}
	var out []Module
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "empty=") {
			continue
		}
		coord, configs, _ := strings.Cut(line, "=")
		parts := strings.Split(coord, ":")
		if len(parts) != 3 {
			continue
		}
		name := parts[0] + ":" + parts[1]
		dev := true
		for _, c := range strings.Split(configs, ",") {
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(c)), "test") {
				dev = false
			}
		}
		out = append(out, Module{Path: name, Version: parts[2], Ecosystem: EcosystemMaven, Dev: dev,
			Indirect: build != "" && !strings.Contains(build, name)})
	}
	return out
}
