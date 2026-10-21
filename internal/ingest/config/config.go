package config

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/model"
)

const maxFileSize = 2 << 20

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".nuxt": true, ".output": true,
	"dist": true, "testdata": true, ".idea": true, ".vscode": true, ".terraform": true,
}

var templateExts = map[string]bool{".tmpl": true, ".gotmpl": true, ".tpl": true, ".gohtml": true}

// Extract indexes YAML files and Go templates under root.
func Extract(ctx context.Context, repo, root string) (*model.Graph, error) {
	b := &builder{repo: repo, g: model.NewGraph()}
	b.g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo})

	charts := map[string]bool{} // chart roots (repo-relative, slash)
	type file struct{ rel, abs string }
	var yamls, templates []file
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p == root {
				return nil
			}
			name := d.Name()
			// .github is the one hidden directory worth indexing (workflows).
			if skipDirs[name] || (strings.HasPrefix(name, ".") && name != ".github") || isNestedRepo(p) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		name := d.Name()
		if name == "Chart.yaml" {
			charts[path.Dir(rel)] = true
		}
		ext := strings.ToLower(filepath.Ext(name))
		switch {
		case templateExts[ext]:
			templates = append(templates, file{rel, p})
		case ext == ".yaml" || ext == ".yml":
			yamls = append(yamls, file{rel, p})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking config files: %w", err)
	}

	chartOf := func(rel string) string {
		for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if charts[dir] {
				return dir
			}
		}
		if charts["."] {
			return "."
		}
		return ""
	}
	valuesFiles := map[string]string{}
	refs := &templateRefs{}
	for _, f := range yamls {
		content, ok := read(f.abs)
		if !ok {
			continue
		}
		chart := chartOf(f.rel)
		// Files under a chart's templates/ are Go templates producing YAML.
		if chart != "" && strings.HasPrefix(f.rel, path.Join(chart, "templates")+"/") {
			b.templateFile(f.rel, content, chart, refs)
			continue
		}
		isValues := chart != "" && f.rel == path.Join(chart, "values.yaml")
		if isValues {
			valuesFiles[chart] = f.rel
		}
		b.yamlFile(f.rel, content, isValues)
	}
	for _, f := range templates {
		if content, ok := read(f.abs); ok {
			b.templateFile(f.rel, content, chartOf(f.rel), refs)
		}
	}
	b.resolve(refs, valuesFiles)
	return b.g, nil
}

func read(p string) ([]byte, bool) {
	info, err := os.Stat(p)
	if err != nil || info.Size() > maxFileSize {
		return nil, false
	}
	b, err := os.ReadFile(p) //nolint:gosec // inside root, not a symlink
	return b, err == nil
}

func isNestedRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}
