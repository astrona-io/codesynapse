package golang

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"golang.org/x/tools/go/packages"
)

// Every module under the root is loaded, each from its own directory: "./..."
// stops at the root module's boundary, so nested modules (a tools/ or api/
// module with its own go.mod) would be silently left out. The walk is the same
// with or without a repository policy; a policy only adds exclusions, which
// also keep ignored directories out of the initial load. Imports can still be
// read by the type checker; emitted files are filtered too.
func loadPackages(ctx context.Context, cfg *packages.Config, patterns []string, policy *repoconfig.Policy) ([]*packages.Package, error) {
	if len(patterns) != 1 || patterns[0] != "./..." {
		return packages.Load(cfg, patterns...)
	}
	const maxPackageRoots = 4096
	modules, dirs := map[string]bool{}, map[string]bool{}
	err := filepath.WalkDir(cfg.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if allowed, err := policy.WalkEntry(cfg.Dir, p, d); !allowed || err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != cfg.Dir && (name == "vendor" || name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			if p != cfg.Dir {
				if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if d.Name() == "go.mod" {
			modules[filepath.Dir(p)] = true
		}
		if strings.HasSuffix(d.Name(), ".go") {
			dirs[filepath.Dir(p)] = true
		}
		if len(dirs)+len(modules) > maxPackageRoots {
			return fmt.Errorf("go package enumeration exceeds %d roots", maxPackageRoots)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	groups := map[string][]string{}
	for dir := range dirs {
		for ancestor := dir; ; ancestor = filepath.Dir(ancestor) {
			if modules[ancestor] {
				rel, err := filepath.Rel(ancestor, dir)
				if err != nil {
					return nil, err
				}
				groups[ancestor] = append(groups[ancestor], "./"+filepath.ToSlash(rel))
				break
			}
			if ancestor == cfg.Dir || filepath.Dir(ancestor) == ancestor {
				break
			}
		}
	}
	roots := make([]string, 0, len(groups))
	for root := range groups {
		roots = append(roots, root)
	}
	slices.Sort(roots)
	var all []*packages.Package
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		copyConfig := *cfg
		copyConfig.Dir = root
		slices.Sort(groups[root])
		loaded, err := packages.Load(&copyConfig, groups[root]...)
		if err != nil {
			return nil, err
		}
		all = append(all, loaded...)
	}
	return all, nil
}
