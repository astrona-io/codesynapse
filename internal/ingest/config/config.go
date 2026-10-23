package config

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/model"
)

const maxFileSize = 2 << 20

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".nuxt": true, ".output": true,
	"dist": true, "testdata": true, ".idea": true, ".vscode": true, ".terraform": true,
}

var templateExts = map[string]bool{".tmpl": true, ".gotmpl": true, ".tpl": true, ".gohtml": true}

// Extract indexes YAML files and Go templates under root.
func Extract(ctx context.Context, repo, root string) (*model.Graph, error) {
	b := &builder{repo: repo, g: model.NewGraph(), policy: repoconfig.FromContext(ctx)}
	b.g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo})

	charts := map[string]bool{} // chart roots (repo-relative, slash)
	type file struct{ rel, abs string }
	var yamls, templates []file
	var manifests []string
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
		if allowed, policyErr := repoconfig.FromContext(ctx).WalkEntry(root, p, d); !allowed || policyErr != nil {
			return policyErr
		}
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
		if isDependencyManifest(rel) {
			manifests = append(manifests, rel)
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
		content, ok := readObserved(ctx, f.rel, f.abs)
		if !ok {
			continue
		}
		chart := chartOf(f.rel)
		// Files under a chart's templates/ are Go templates producing YAML.
		if chart != "" && strings.HasPrefix(f.rel, path.Join(chart, "templates")+"/") {
			b.templateFile(f.rel, content, chart, refs)
			observeConfig(ctx, b, f.rel)
			continue
		}
		isValues := chart != "" && f.rel == path.Join(chart, "values.yaml")
		if isValues {
			valuesFiles[chart] = f.rel
		}
		b.yamlFile(f.rel, content, isValues)
		observeConfig(ctx, b, f.rel)
	}
	for _, f := range templates {
		if content, ok := readObserved(ctx, f.rel, f.abs); ok {
			b.templateFile(f.rel, content, chartOf(f.rel), refs)
			observeConfig(ctx, b, f.rel)
		}
	}
	b.resolve(refs, valuesFiles)
	b.dependencyUpdateRules(root, manifests)
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

func readObserved(ctx context.Context, rel, filename string) ([]byte, bool) {
	info, err := os.Lstat(filename)
	diagnostic := ""
	switch {
	case err != nil:
		diagnostic = "metadata_unavailable"
	case !info.Mode().IsRegular():
		diagnostic = "special_file"
	case info.Size() > maxFileSize:
		diagnostic = "size_limit"
	}
	if diagnostic != "" {
		ingestreport.Observe(ctx, rel, ingestreport.Config, ingestreport.StageFailed, diagnostic)
		return nil, false
	}
	content, err := os.ReadFile(filename) //nolint:gosec // filename comes from the repository walker
	if err != nil {
		ingestreport.Observe(ctx, rel, ingestreport.Config, ingestreport.StageFailed, "read_failed")
		return nil, false
	}
	return content, true
}
func observeConfig(ctx context.Context, b *builder, rel string) {
	for _, kind := range []model.NodeKind{model.KindConfig, model.KindTemplate} {
		node, _ := b.g.Node(b.id(kind, rel))
		if node == nil {
			continue
		}
		outcome, diagnostic := ingestreport.StageSucceeded, ""
		yamlTemplate := kind == model.KindTemplate && node.Package != "" && (strings.HasSuffix(rel, ".yaml") || strings.HasSuffix(rel, ".yml"))
		if strings.HasPrefix(node.Summary, "unparseable ") || yamlTemplate && node.Language != "yaml" {
			outcome, diagnostic = ingestreport.StageFailed, "parse_failed"
		}
		ingestreport.Observe(ctx, rel, ingestreport.Config, outcome, diagnostic)
	}
	if !b.policy.KubernetesAllowed(rel) {
		ingestreport.Observe(ctx, rel, ingestreport.Kubernetes, ingestreport.StageExcluded, "stage_excluded")
	}
}
