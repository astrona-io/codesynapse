package vuln

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Paths say how an indirect dependency got into the project: the shortest
// chain from a dependency the project declares itself, and how many of those
// lead to it. They are computed from the requirement edges each ecosystem
// records (go mod graph, npm and Python lockfiles) and stay empty where the
// edges are unknown.

const (
	goListTimeout = 120 * time.Second
	// maxRootWalk bounds the work of counting, per direct dependency, what it
	// reaches: roots times edges. Beyond it only the shortest chain is kept.
	maxRootWalk = 5_000_000
	maxViaDepth = 8
)

// GoGraph reads which module needs which, from the packages the build
// actually imports: `go list -deps -test` over every go.mod under root, with
// each import crossing from one module into another counted as an edge from
// the importing module to the imported one. That is what ends up in the
// build, which is more useful than the requirement lines of go.mod files,
// and it works offline once the dependencies are in the module cache (the
// code extraction needs them too). Like the extraction it runs with the
// local toolchain, no cgo and, unless allowNetwork is set, no network. A
// module that cannot be listed contributes what could be loaded, or nothing.
func GoGraph(ctx context.Context, root string, allowNetwork bool) map[string][]string {
	edges := map[string]map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // best effort: paths are an enrichment
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are skipped
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (name == "vendor" || name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") || isRepo(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		for parent, children := range parseGoList(goListDeps(ctx, filepath.Dir(p), allowNetwork)) {
			if edges[parent] == nil {
				edges[parent] = map[string]bool{}
			}
			for _, c := range children {
				edges[parent][c] = true
			}
		}
		return nil
	})
	res := make(map[string][]string, len(edges))
	for parent, set := range edges {
		for c := range set {
			res[parent] = append(res[parent], c)
		}
		slices.Sort(res[parent])
	}
	return res
}

func goListDeps(ctx context.Context, dir string, allowNetwork bool) []byte {
	ctx, cancel := context.WithTimeout(ctx, goListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-deps", "-test", "-json=ImportPath,Imports,Module,Standard", "./...") //nolint:gosec // fixed arguments, no shell
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local", "GOFLAGS=", "GOWORK=off")
	if !allowNetwork {
		cmd.Env = append(cmd.Env, "GOPROXY=off", "GONOSUMDB=*")
	}
	out, _ := cmd.Output() //nolint:errcheck // -e reports package errors in the stream; what loaded is still useful
	return out
}

// parseGoList reads the JSON stream of `go list -deps -json` into importing
// module path to the module paths its packages import. Packages of the main
// module are left out as importers: what the project requires itself is known
// from go.mod. Test variants ("p [p.test]") count as the package they test.
func parseGoList(out []byte) map[string][]string {
	type pkg struct {
		ImportPath string
		Imports    []string
		Standard   bool
		Module     *struct {
			Path string
			Main bool
		}
	}
	var pkgs []pkg
	module := map[string]string{} // import path -> module path
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p pkg
		if dec.Decode(&p) != nil {
			break
		}
		pkgs = append(pkgs, p)
		if p.Module != nil && !p.Standard {
			module[stripVariant(p.ImportPath)] = p.Module.Path
		}
	}
	edges := map[string][]string{}
	for _, p := range pkgs {
		if p.Module == nil || p.Module.Main || p.Standard {
			continue
		}
		for _, imp := range p.Imports {
			to := module[stripVariant(imp)]
			if to != "" && to != p.Module.Path && !slices.Contains(edges[p.Module.Path], to) {
				edges[p.Module.Path] = append(edges[p.Module.Path], to)
			}
		}
	}
	return edges
}

// stripVariant turns "p [p.test]" into "p".
func stripVariant(importPath string) string {
	p, _, _ := strings.Cut(importPath, " [")
	return p
}

// AssignPaths sets Via and Roots on the indirect modules of mods. goEdges
// are the Go requirement edges (GoGraph); the other ecosystems carry theirs
// in Module.Requires.
func AssignPaths(mods []Module, goEdges map[string][]string) {
	name := map[string]string{} // key -> display name
	direct := map[string]bool{}
	edges := map[string][]string{}
	for _, m := range mods {
		k := m.Key()
		name[k] = m.Path
		if !m.Indirect {
			direct[k] = true
		}
		edges[k] = append(edges[k], m.Requires...)
		if m.ecosystem() == EcosystemGo {
			edges[k] = append(edges[k], goEdges[m.Path]...)
		}
	}
	// Shortest chain from any direct dependency: a breadth-first walk from
	// all of them at once, remembering who reached each module first.
	parent := map[string]string{}
	var queue []string
	for _, m := range mods {
		if k := m.Key(); direct[k] && !slices.Contains(queue, k) {
			queue = append(queue, k)
			parent[k] = ""
		}
	}
	slices.Sort(queue)
	for head := 0; head < len(queue); head++ {
		for _, c := range edges[queue[head]] {
			if _, seen := parent[c]; seen || name[c] == "" {
				continue
			}
			parent[c] = queue[head]
			queue = append(queue, c)
		}
	}
	via := map[string]string{}
	for k := range parent {
		if direct[k] {
			continue
		}
		var chain []string
		for p := parent[k]; p != "" && len(chain) < maxViaDepth; p = parent[p] {
			chain = append(chain, name[p])
		}
		slices.Reverse(chain)
		via[k] = strings.Join(chain, " > ")
	}
	// How many direct dependencies lead to each module.
	roots := map[string]int{}
	edgeCount := 0
	for _, e := range edges {
		edgeCount += len(e)
	}
	if len(direct)*max(edgeCount, 1) <= maxRootWalk {
		for d := range direct {
			seen := map[string]bool{d: true}
			stack := []string{d}
			for len(stack) > 0 {
				cur := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				for _, c := range edges[cur] {
					if !seen[c] && name[c] != "" {
						seen[c] = true
						stack = append(stack, c)
						roots[c]++
					}
				}
			}
		}
	}
	for i, m := range mods {
		if !m.Indirect {
			continue
		}
		mods[i].Via, mods[i].Roots = via[m.Key()], roots[m.Key()]
	}
}
