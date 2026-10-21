package api

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// maxScanFiles bounds the content scan of one source directory so the
// Sources list stays fast on large monorepos.
const maxScanFiles = 20000

var scanSkip = map[string]bool{
	"node_modules": true, "vendor": true, ".git": true, ".nuxt": true, ".output": true, ".next": true,
	"dist": true, "build": true, "__pycache__": true, ".venv": true, "venv": true, "testdata": true,
}

// scanContent counts files per format below dir, the same formats the
// ingest pipeline indexes. Helm charts are counted once per Chart.yaml.
func scanContent(dir string) (map[string]int, bool) {
	counts := map[string]int{}
	seen := 0
	truncated := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // best-effort scan
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries
		}
		if d.IsDir() {
			name := d.Name()
			if p != dir && (scanSkip[name] || (strings.HasPrefix(name, ".") && name != ".github")) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if seen++; seen > maxScanFiles {
			truncated = true
			return filepath.SkipAll
		}
		name := d.Name()
		rel := filepath.ToSlash(p)
		switch {
		case name == "Chart.yaml":
			counts["helm"]++
		case strings.Contains(rel, "/.github/workflows/") && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")):
			counts["github-actions"]++
		case strings.HasPrefix(name, "docker-compose") || strings.HasPrefix(name, "compose."):
			counts["compose"]++
		case name == "package-lock.json" || name == "yarn.lock" || name == "pnpm-lock.yaml":
			counts["npm"]++
		case strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt"), name == "poetry.lock", name == "uv.lock", name == "Pipfile.lock":
			counts["pypi"]++
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".go":
			counts["go"]++
		case ".py":
			counts["python"]++
		case ".ts", ".tsx", ".mts", ".cts":
			if !strings.HasSuffix(name, ".d.ts") {
				counts["typescript"]++
			}
		case ".js", ".jsx", ".mjs", ".cjs":
			counts["javascript"]++
		case ".vue":
			counts["vue"]++
		case ".yaml", ".yml":
			counts["yaml"]++
		case ".md", ".markdown", ".mdx":
			counts["markdown"]++
		case ".tmpl", ".gotmpl", ".tpl":
			counts["go-template"]++
		}
		return nil
	})
	return counts, truncated
}
