package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/model"
)

// renovateFiles are the config locations Renovate checks, in its own order;
// the first one found is the one Renovate uses. Root package.json comes last.
var renovateFiles = []string{
	"renovate.json", "renovate.json5",
	".github/renovate.json", ".github/renovate.json5",
	".gitlab/renovate.json", ".gitlab/renovate.json5",
	".renovaterc", ".renovaterc.json", ".renovaterc.json5",
}

var dependabotFiles = []string{".github/dependabot.yml", ".github/dependabot.yaml"}

// anchorManifests decide where the missing-automation finding is shown when
// the repository root has several manifests.
var anchorManifests = []string{
	"go.mod", "package.json", "pyproject.toml", "requirements.txt", "Pipfile",
	"Dockerfile", "Chart.yaml", "compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml",
}

// isDependencyManifest reports files that pin third-party code something
// should keep up to date: package manifests, images and CI actions.
func isDependencyManifest(rel string) bool {
	name := baseName(rel)
	yml := strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
	switch {
	case name == "go.mod", name == "package.json", name == "pyproject.toml", name == "Pipfile", name == "Chart.yaml":
		return true
	case strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt"):
		return true
	case isDockerfile(name):
		return true
	case yml && (isCompose(rel) || isWorkflow(rel)):
		return true
	}
	return false
}

func isDockerfile(name string) bool {
	return name == "Dockerfile" || name == "Containerfile" || strings.HasPrefix(name, "Dockerfile.") || strings.HasSuffix(name, ".Dockerfile")
}

// regularFile reads root/rel only if it is a regular file, so a planted
// symlink cannot point the reader outside the repository.
func regularFile(root, rel string) ([]byte, bool) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	return read(p)
}

// dependencyUpdateRules flags repositories with manifests but no update
// automation, and checks the Renovate config when there is one. Findings
// hang off the repository node: whether updates are automated is a
// property of the repository, and the files involved (go.mod, JSON) have no
// node of their own here.
func (b *builder) dependencyUpdateRules(root string, manifests []string) {
	if len(manifests) == 0 {
		return
	}
	repoID := model.NodeID(b.repo, model.KindRepository, b.repo)
	for _, rel := range renovateFiles {
		if content, ok := regularFile(root, rel); ok {
			b.renovateRules(repoID, rel, content, nil, manifests)
			return
		}
	}
	if content, ok := regularFile(root, "package.json"); ok {
		if pkg, err := parseJSON5(content); err == nil {
			if f := pkg.field("renovate"); f != nil {
				b.renovateRules(repoID, "package.json", nil, f, manifests)
				return
			}
		}
	}
	for _, rel := range dependabotFiles {
		if _, ok := regularFile(root, rel); ok {
			return
		}
	}
	anchor := manifestAnchor(manifests)
	b.report(anchor, repoID, "dependency-updates-missing", 1,
		fmt.Sprintf("%d dependency manifests (such as %s) and no Renovate or Dependabot config", len(manifests), anchor))
}

// manifestAnchor picks the most representative manifest: a well-known one
// at the root, else the shallowest.
func manifestAnchor(manifests []string) string {
	for _, name := range anchorManifests {
		if slices.Contains(manifests, name) {
			return name
		}
	}
	return slices.MinFunc(manifests, func(a, b string) int {
		if d := strings.Count(a, "/") - strings.Count(b, "/"); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
}

// builtinPresetPrefixes are Renovate's own preset namespaces. Any other
// preset (github>org/repo, local>..., npm packages) is fetched at run time,
// so settings it may provide cannot be judged from this repository.
var builtinPresetPrefixes = []string{
	"config:", ":", "default:", "helpers:", "docker:", "security:", "schedule:", "group:", "packages:",
	"replacements:", "workarounds:", "monorepo:", "npm:", "preview:", "customManagers:", "regexManagers:",
	"mergeConfidence:", "abandonments:",
}

func builtinPreset(p string) bool {
	for _, pre := range builtinPresetPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// wildcardMatchers are package matchers whose wildcard value selects every
// dependency.
var wildcardMatchers = map[string]bool{
	"matchPackageNames": true, "matchPackagePatterns": true, "matchDepNames": true, "matchDepPatterns": true,
}

// matchesAll reports a packageRules entry that applies to every dependency:
// no matchers at all, or only wildcard package matchers.
func matchesAll(rule *jsonValue) bool {
	for _, f := range rule.fields {
		if !strings.HasPrefix(f.key, "match") && !strings.HasPrefix(f.key, "exclude") {
			continue
		}
		if !wildcardMatchers[f.key] {
			return false
		}
		if !slices.ContainsFunc(f.val.strings(), func(s string) bool {
			return s == "*" || s == "**" || s == ".*" || s == "/.*/" || s == "/.+/"
		}) {
			return false
		}
	}
	return true
}

// hasReleaseAge reports a minimumReleaseAge (or the legacy stabilityDays)
// that actually delays anything.
func hasReleaseAge(v *jsonValue) bool {
	if a := v.get("minimumReleaseAge"); a != nil && a.kind == jsonString {
		s := strings.TrimSpace(a.str)
		if s != "" && !strings.HasPrefix(s, "0") {
			return true
		}
	}
	if d := v.get("stabilityDays"); d != nil && d.kind == jsonNumber && d.str != "0" {
		return true
	}
	return false
}

// automergeSections are update-type and lock-file blocks that take their
// own automerge setting. vulnerabilityAlerts is left out on purpose:
// Renovate ignores the release age for security fixes anyway.
var automergeSections = []string{"major", "minor", "patch", "pin", "digest", "pinDigest", "lockFileMaintenance"}

// renovateRules checks one Renovate config. Either content (a config file)
// or embedded (the "renovate" key of package.json) is set.
func (b *builder) renovateRules(repoID, rel string, content []byte, embedded *jsonField, manifests []string) {
	var cfg *jsonValue
	if embedded != nil {
		cfg = embedded.val
	} else {
		v, err := parseJSON5(content)
		if err != nil {
			line := 1
			if je := (*jsonError)(nil); errors.As(err, &je) {
				line = je.line
			}
			b.report(rel, repoID, "renovate-config-invalid", line, rel+" is not valid JSON/JSON5")
			return
		}
		cfg = v
	}
	if cfg.kind != jsonObject {
		b.report(rel, repoID, "renovate-config-invalid", cfg.line, rel+": the Renovate config is not an object")
		return
	}

	if f := cfg.field("enabled"); f != nil && f.val.isBool(false) {
		// Nothing else matters while Renovate does not run.
		b.report(rel, repoID, "renovate-disabled", f.line, "enabled: false turns Renovate off for the repository")
		return
	}
	rules := cfg.get("packageRules")
	var ruleList []*jsonValue
	if rules != nil {
		ruleList = rules.items
	}
	for i, r := range ruleList {
		if f := r.field("enabled"); f != nil && f.val.isBool(false) && matchesAll(r) {
			b.report(rel, repoID, "renovate-disabled", f.line, fmt.Sprintf("packageRules[%d] disables updates for every dependency", i))
		}
	}
	if va := cfg.get("vulnerabilityAlerts"); va != nil {
		if f := va.field("enabled"); f != nil && f.val.isBool(false) {
			b.report(rel, repoID, "renovate-vulnerability-alerts-off", f.line, "vulnerabilityAlerts.enabled: false stops security update PRs")
		}
	}

	extends := cfg.get("extends")
	external := false
	var presets []string
	if extends != nil {
		for _, it := range extends.items {
			if it.kind == jsonString {
				presets = append(presets, it.str)
				external = external || !builtinPreset(it.str)
			}
		}
	}
	// A shared preset from elsewhere may set the release age or digest
	// pinning, so absence checks only run on self-contained configs.
	if external {
		return
	}
	bestPractices := slices.Contains(presets, "config:best-practices")
	b.automergeRules(rel, repoID, cfg, extends, ruleList, presets, bestPractices)
	b.digestRules(rel, repoID, cfg, ruleList, presets, bestPractices, manifests)
}

func (b *builder) automergeRules(rel, repoID string, cfg, extends *jsonValue, rules []*jsonValue, presets []string, bestPractices bool) {
	covered := hasReleaseAge(cfg) || bestPractices || slices.Contains(presets, "security:minimumReleaseAgeNpm") ||
		slices.ContainsFunc(rules, func(r *jsonValue) bool { return matchesAll(r) && hasReleaseAge(r) })
	if covered {
		return
	}
	const detail = "%s enables automerge without minimumReleaseAge"
	if f := cfg.field("automerge"); f != nil && f.val.isBool(true) {
		b.report(rel, repoID, "renovate-automerge-no-release-age", f.line, fmt.Sprintf(detail, "automerge: true"))
	}
	if extends != nil {
		for _, it := range extends.items {
			if it.kind == jsonString && automergePreset(it.str) {
				b.report(rel, repoID, "renovate-automerge-no-release-age", it.line, fmt.Sprintf(detail, "preset "+it.str))
			}
		}
	}
	for _, s := range automergeSections {
		sec := cfg.get(s)
		if f := sec.field("automerge"); f != nil && f.val.isBool(true) && !hasReleaseAge(sec) {
			b.report(rel, repoID, "renovate-automerge-no-release-age", f.line, fmt.Sprintf(detail, s+".automerge"))
		}
	}
	for i, r := range rules {
		if f := r.field("automerge"); f != nil && f.val.isBool(true) && !hasReleaseAge(r) {
			b.report(rel, repoID, "renovate-automerge-no-release-age", f.line, fmt.Sprintf(detail, fmt.Sprintf("packageRules[%d]", i)))
		}
	}
}

// automergePresets are Renovate's built-in presets that turn automerge on;
// :automergeBranch and :automergePr only choose how, so they are not here.
var automergePresets = map[string]bool{
	"automergeAll": true, "automergeMajor": true, "automergeMinor": true, "automergePatch": true,
	"automergeDigest": true, "automergeStableNonMajor": true, "automergeLinters": true,
	"automergeTesters": true, "automergeTypes": true,
}

func automergePreset(p string) bool {
	name, ok := strings.CutPrefix(p, ":")
	if !ok {
		name, ok = strings.CutPrefix(p, "default:")
	}
	return ok && automergePresets[name]
}

func (b *builder) digestRules(rel, repoID string, cfg *jsonValue, rules []*jsonValue, presets []string, bestPractices bool, manifests []string) {
	var actions, docker bool
	for _, m := range manifests {
		actions = actions || isWorkflow(m)
		docker = docker || isDockerfile(baseName(m)) || isCompose(m)
	}
	enabled := cfg.get("enabledManagers").strings()
	managed := func(names ...string) bool {
		for _, n := range names {
			if (enabled == nil || slices.Contains(enabled, n)) && !cfg.get(n).get("enabled").isBool(false) {
				return true
			}
		}
		return false
	}
	anyRulePins := slices.ContainsFunc(rules, func(r *jsonValue) bool { return r.get("pinDigests").isBool(true) })
	pins := func(preset string, managers ...string) bool {
		if bestPractices || anyRulePins || cfg.get("pinDigests").isBool(true) || slices.Contains(presets, preset) {
			return true
		}
		return slices.ContainsFunc(managers, func(m string) bool { return cfg.get(m).get("pinDigests").isBool(true) })
	}
	var missing []string
	if actions && managed("github-actions") && !pins("helpers:pinGitHubActionDigests", "github-actions") {
		missing = append(missing, "GitHub Actions")
	}
	if docker && managed("dockerfile", "docker-compose") && !pins("docker:pinDigests", "dockerfile", "docker-compose") {
		missing = append(missing, "Docker images")
	}
	if len(missing) == 0 {
		return
	}
	line := cfg.line
	if f := cfg.field("extends"); f != nil {
		line = f.line
	}
	b.report(rel, repoID, "renovate-no-digest-pinning", line, strings.Join(missing, " and ")+" are updated by tag without digest pinning")
}
