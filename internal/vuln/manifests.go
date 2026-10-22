package vuln

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/parisnakitakejser/codesynapse/internal/license"
)

// Ecosystems, as named by OSV.
const (
	EcosystemGo   = "Go"
	EcosystemNPM  = "npm"
	EcosystemPyPI = "PyPI"
	// The ecosystems below are read from lockfiles only, and OSV names them so.
	EcosystemCargo     = "crates.io"
	EcosystemPackagist = "Packagist"
	EcosystemRubyGems  = "RubyGems"
	EcosystemNuGet     = "NuGet"
	EcosystemMaven     = "Maven"
)

// maxModules bounds the dependencies read from lockfiles.
const maxModules = 20000

const maxManifest = 32 << 20

// lockfiles lists the manifests read, by file name.
var lockfiles = map[string]func([]byte, string) []Module{
	"package-lock.json":  parseNPMLock,
	"yarn.lock":          parseYarnLock,
	"pnpm-lock.yaml":     parsePNPMLock,
	"poetry.lock":        parseTOMLLock,
	"uv.lock":            parseTOMLLock,
	"Pipfile.lock":       parsePipfileLock,
	"Cargo.lock":         parseCargoLock,
	"composer.lock":      parseComposerLock,
	"Gemfile.lock":       parseGemfileLock,
	"packages.lock.json": parseNuGetLock,
	"gradle.lockfile":    parseGradleLock,
}

// ManifestModules reads npm, Python, Rust, PHP, Ruby, .NET and Gradle lockfiles under root. dir is the
// lockfile's directory, used to find direct dependencies next to it.
func ManifestModules(root string) ([]Module, error) {
	seen := map[string]Module{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (name == "node_modules" || name == "vendor" || name == "testdata" || name == "target" || name == ".venv" || name == "venv" || strings.HasPrefix(name, ".") || isRepo(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		parse, ok := lockfiles[name]
		isReq := strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt")
		if !ok && !isReq {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxManifest {
			return nil //nolint:nilerr // skip unreadable or huge files
		}
		data, err := os.ReadFile(p) //nolint:gosec // inside root, not a symlink
		if err != nil {
			return nil //nolint:nilerr // skip unreadable files
		}
		var mods []Module
		if isReq {
			mods = parseRequirements(data)
		} else {
			mods = parse(data, filepath.Dir(p))
		}
		for _, m := range mods {
			key := m.Ecosystem + "|" + m.Path + "|" + m.Version
			if prev, ok := seen[key]; ok {
				// Shipped by any lockfile means shipped; keep a licence either one recorded.
				prev.Dev = prev.Dev && m.Dev
				for _, r := range m.Requires {
					if !slices.Contains(prev.Requires, r) {
						prev.Requires = append(prev.Requires, r)
					}
				}
				if prev.License == "" {
					prev.License, prev.LicenseSource = m.License, m.LicenseSource
				}
				if !prev.Indirect {
					seen[key] = prev
					continue
				}
				m.Dev, m.License, m.LicenseSource = prev.Dev, cmp.Or(m.License, prev.License), cmp.Or(m.LicenseSource, prev.LicenseSource)
				m.Requires = prev.Requires
			}
			if len(seen) >= maxModules {
				return filepath.SkipAll
			}
			seen[key] = m
		}
		return nil
	})
	out := make([]Module, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	return out, err
}

// npmDirect returns the dependency names declared in package.json in dir.
func npmDirect(dir string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // next to a lockfile inside root
	if err != nil {
		return nil
	}
	var pkg map[string]json.RawMessage
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	direct := map[string]bool{}
	for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		var deps map[string]string
		if json.Unmarshal(pkg[field], &deps) == nil {
			for name := range deps {
				direct[name] = true
			}
		}
	}
	return direct
}

func parseNPMLock(data []byte, dir string) []Module {
	var lock struct {
		Packages     map[string]json.RawMessage `json:"packages"`
		Dependencies map[string]json.RawMessage `json:"dependencies"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return nil
	}
	direct := npmDirect(dir)
	var out []Module
	for key, raw := range lock.Packages {
		var p struct {
			Version              string            `json:"version"`
			Link                 bool              `json:"link"`
			Dev                  bool              `json:"dev"`
			DevOptional          bool              `json:"devOptional"`
			Dependencies         map[string]string `json:"dependencies"`
			OptionalDependencies map[string]string `json:"optionalDependencies"`
		}
		i := strings.LastIndex(key, "node_modules/")
		if key == "" || i < 0 || json.Unmarshal(raw, &p) != nil || p.Version == "" || p.Link {
			continue
		}
		name := key[i+len("node_modules/"):]
		m := Module{Path: name, Version: p.Version, Ecosystem: EcosystemNPM, Indirect: !direct[name] || strings.Count(key, "node_modules/") > 1,
			Dev: p.Dev || p.DevOptional, Requires: npmRequires(p.Dependencies, p.OptionalDependencies)}
		if id := license.NPMField(raw); id != "" {
			m.License, m.LicenseSource = id, "package-lock.json"
		}
		out = append(out, m)
	}
	if len(lock.Packages) == 0 { // lockfileVersion 1
		var walk func(deps map[string]json.RawMessage, nested bool)
		walk = func(deps map[string]json.RawMessage, nested bool) {
			for name, raw := range deps {
				var d struct {
					Version      string                     `json:"version"`
					Dependencies map[string]json.RawMessage `json:"dependencies"`
				}
				if json.Unmarshal(raw, &d) != nil || d.Version == "" {
					continue
				}
				out = append(out, Module{Path: name, Version: d.Version, Ecosystem: EcosystemNPM, Indirect: nested || !direct[name]})
				walk(d.Dependencies, true)
			}
		}
		walk(lock.Dependencies, false)
	}
	return out
}

// npmRequires turns the dependency maps of a lockfile entry into module keys.
func npmRequires(maps ...map[string]string) []string {
	var out []string
	for _, deps := range maps {
		for name := range deps {
			if k := (Module{Path: name, Ecosystem: EcosystemNPM}).Key(); !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	slices.Sort(out)
	return out
}

var yarnEntry = regexp.MustCompile(`^"?(@?[^@"\s]+)@`)

func parseYarnLock(data []byte, dir string) []Module {
	direct := npmDirect(dir)
	var out []Module
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	name := ""
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			name = ""
			if m := yarnEntry.FindStringSubmatch(line); m != nil {
				name = m[1]
			}
			continue
		}
		t := strings.TrimSpace(line)
		if name != "" && (strings.HasPrefix(t, "version ") || strings.HasPrefix(t, "version:")) {
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(t, "version:"), "version")), `"`)
			out = append(out, Module{Path: name, Version: v, Ecosystem: EcosystemNPM, Indirect: !direct[name]})
			name = ""
		}
	}
	return out
}

var pnpmKey = regexp.MustCompile(`^/?(@?[^@/]+(?:/[^@/]+)?)@([^(@]+)`)

func parsePNPMLock(data []byte, dir string) []Module {
	var lock struct {
		Packages map[string]any `yaml:"packages"`
	}
	if yaml.Unmarshal(data, &lock) != nil {
		return nil
	}
	direct := npmDirect(dir)
	var out []Module
	for key := range lock.Packages {
		if m := pnpmKey.FindStringSubmatch(key); m != nil {
			out = append(out, Module{Path: m[1], Version: m[2], Ecosystem: EcosystemNPM, Indirect: !direct[m[1]]})
		}
	}
	return out
}

var reqLine = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)(\[[^\]]*\])?\s*==\s*([A-Za-z0-9._+!-]+)`)

// parseRequirements reads pinned "name==version" lines; ranges cannot be
// checked against advisories and are skipped.
func parseRequirements(data []byte) []Module {
	var out []Module
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if m := reqLine.FindStringSubmatch(line); m != nil {
			out = append(out, Module{Path: normalizePy(m[1]), Version: m[3], Ecosystem: EcosystemPyPI})
		}
	}
	return out
}

var (
	tomlPackage = regexp.MustCompile(`(?m)^\[\[package\]\]\s*$`)
	tomlName    = regexp.MustCompile(`(?m)^name\s*=\s*"([^"]+)"`)
	tomlVersion = regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`)
	pyDepName   = regexp.MustCompile(`["']([A-Za-z0-9][A-Za-z0-9._-]*)\s*(?:\[[^\]]*\])?\s*(?:[<>=!~;].*)?["']|^\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*=`)
)

// parseTOMLLock reads poetry.lock and uv.lock: [[package]] blocks with
// name and version. Direct dependencies come from pyproject.toml.
func parseTOMLLock(data []byte, dir string) []Module {
	direct := pyprojectDirect(dir)
	var out []Module
	for _, block := range tomlPackage.Split(string(data), -1)[1:] {
		n, v := tomlName.FindStringSubmatch(block), tomlVersion.FindStringSubmatch(block)
		if n == nil || v == nil || strings.Contains(block, "source = { editable") || strings.Contains(block, "source = { virtual") {
			continue
		}
		name := normalizePy(n[1])
		out = append(out, Module{Path: name, Version: v[1], Ecosystem: EcosystemPyPI, Indirect: direct != nil && !direct[name], Requires: tomlRequires(block)})
	}
	return out
}

var (
	uvDeps       = regexp.MustCompile(`(?s)\ndependencies = \[(.*?)\n\]`)
	uvDepName    = regexp.MustCompile(`\{\s*name\s*=\s*"([^"]+)"`)
	poetryDeps   = regexp.MustCompile(`(?s)\[package\.dependencies\]\n(.*?)(?:\n\[|$)`)
	poetryDepKey = regexp.MustCompile(`(?m)^([A-Za-z0-9][A-Za-z0-9._-]*)\s*=`)
)

// tomlRequires reads the runtime requirements of one [[package]] block: the
// dependencies array of uv.lock or the [package.dependencies] table of
// poetry.lock.
func tomlRequires(block string) []string {
	var names []string
	if m := uvDeps.FindStringSubmatch(block); m != nil {
		for _, d := range uvDepName.FindAllStringSubmatch(m[1], -1) {
			names = append(names, d[1])
		}
	}
	if m := poetryDeps.FindStringSubmatch(block); m != nil {
		for _, d := range poetryDepKey.FindAllStringSubmatch(m[1], -1) {
			names = append(names, d[1])
		}
	}
	var out []string
	for _, n := range names {
		if n = normalizePy(n); n == "python" {
			continue
		}
		if k := (Module{Path: n, Ecosystem: EcosystemPyPI}).Key(); !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func pyprojectDirect(dir string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml")) //nolint:gosec // next to a lockfile inside root
	if err != nil {
		return nil
	}
	direct := map[string]bool{}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			section = t
			continue
		}
		inDeps := strings.Contains(section, "dependencies") || section == "[project]" && strings.HasPrefix(t, "dependencies")
		if !inDeps {
			continue
		}
		for _, m := range pyDepName.FindAllStringSubmatch(t, -1) {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if name != "" && name != "python" && name != "dependencies" {
				direct[normalizePy(name)] = true
			}
		}
	}
	return direct
}

func parsePipfileLock(data []byte, _ string) []Module {
	var lock map[string]map[string]struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return nil
	}
	var out []Module
	for _, section := range []string{"default", "develop"} {
		for name, p := range lock[section] {
			if v := strings.TrimPrefix(p.Version, "=="); v != "" {
				out = append(out, Module{Path: normalizePy(name), Version: v, Ecosystem: EcosystemPyPI, Dev: section == "develop"})
			}
		}
	}
	return out
}

// normalizePy applies PEP 503 name normalisation.
func normalizePy(name string) string {
	name = strings.ToLower(name)
	return regexp.MustCompile(`[-_.]+`).ReplaceAllString(name, "-")
}

// pyImportAliases maps distribution names to their import names where they differ.
var pyImportAliases = map[string]string{
	"pyyaml": "yaml", "beautifulsoup4": "bs4", "pillow": "PIL", "scikit-learn": "sklearn", "opencv-python": "cv2",
	"python-dateutil": "dateutil", "pyjwt": "jwt", "python-dotenv": "dotenv", "protobuf": "google", "attrs": "attr",
	"pycryptodome": "Crypto", "psycopg2-binary": "psycopg2", "msgpack-python": "msgpack", "python-multipart": "multipart",
}

// ImportName returns the name a module is imported as in source code.
func ImportName(m Module) string {
	switch m.Ecosystem {
	case EcosystemPyPI:
		if a, ok := pyImportAliases[m.Path]; ok {
			return a
		}
		return strings.ReplaceAll(m.Path, "-", "_")
	default:
		return m.Path
	}
}
