package api

import (
	"cmp"
	"context"
	"errors"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

const (
	// maxBackEndRanges bounds the ranges off the prompt's side (the back end
	// of a prompt about the UI, code for one about the documentation) once
	// that side was found.
	maxBackEndRanges = 3
	// maxNamedPaths bounds the file paths of a prompt looked up by name.
	maxNamedPaths = 8
	// maxAreaWords bounds the prompt words checked for naming a package or
	// file.
	maxAreaWords = 40
	// maxAreas bounds the areas one plan follows.
	maxAreas = 6
	// maxAreaRoutes bounds the API routes of one area whose handlers join it.
	maxAreaRoutes = 4
	// maxAreaModules bounds the modules of one area directory that join it.
	maxAreaModules = 8
	// maxRangesPerFile bounds the ranges one file takes in a plan.
	maxRangesPerFile = 2
)

// briefPlan picks the ranges for prompt from the graph alone, in this order:
//
//  1. Files the prompt names by path (mkdocs.yml, docs/hooks/x.py), which the
//     name search does not rank: their names are the base name only.
//  2. Code the prompt names exactly (agentctx.NamesCode), on the prompt's side.
//  3. Areas: what lies in a package, directory or file the prompt names by a
//     plain word ("the gateway", "the stats page", "queue"), taken in turn
//     from each area in the order the prompt names them, with the handlers of
//     the area's API routes and the modules of an area directory. A word that
//     names a part of the code base says where to look better than the
//     generic words around it ("models", "default", "graph", "agents"), which
//     match names all over it (PLAN item 18, cases i to k).
//  4. Everything else minimal_context found.
//
// The prompt's side: a prompt about the documentation (asklocal.AboutDocs)
// puts its pages, site configuration and hooks first, and its "page" is not
// the UI (case h); one about the user interface (asklocal.AboutUI) puts
// front-end files first (case g). Once that side was found, back-end types
// that only share a word are dropped and few other ranges kept. No file takes
// more than maxRangesPerFile ranges. Quoted text is looked up by the client
// in its checkout, since the graph holds no literal text.
func briefPlan(ctx context.Context, st store.Reader, repo, prompt string) (BriefPlan, error) {
	r := store.ForRepository(st, repo)
	docs := asklocal.AboutDocs(prompt)
	ui := !docs && asklocal.AboutUI(prompt)
	side := func(string) bool { return true }
	switch {
	case docs:
		side = asklocal.DocsFile
	case ui:
		side = asklocal.FrontEnd
	}
	areas, err := promptAreas(ctx, r, repo, prompt, docs)
	if err != nil {
		return BriefPlan{}, err
	}
	budget := 4000
	if ui || docs || len(areas) > 0 {
		// A deeper pool, so matches on the prompt's side or in its areas
		// ranked below the first dozen generic names can still lead; the
		// plan stays maxBriefRanges.
		budget = 3 * budget
	}
	mc, err := agentctx.Service{R: r}.MinimalContext(ctx, repo, prompt, nil, budget)
	if err != nil {
		return BriefPlan{}, err
	}
	named, err := namedFiles(ctx, r, repo, prompt)
	if err != nil {
		return BriefPlan{}, err
	}

	codeNamed := agentctx.NamesCode(prompt)
	terms := promptTerms(prompt)
	// covers ranks a range by the words of the prompt its name holds.
	covers := map[string]int{}
	// held: some range in the area holds another word of the prompt.
	held := make([]bool, len(areas))
	var exact, rest, other []string
	// Per area: on the prompt's side, then off it.
	on := make([][]string, len(areas))
	off := make([][]string, len(areas))
	found := slices.ContainsFunc(named, func(loc string) bool { return side(rangePath(loc)) })
	for _, it := range mc.Items {
		if it.Read == "" {
			continue
		}
		p := rangePath(it.Read)
		s := side(p)
		found = found || s
		a := -1
		if !isTestPath(p) && (docs || !asklocal.DocsFile(p)) {
			// A page under docs/ is not in the code area "docs"
			// (internal/ingest/docs) unless the prompt is about the
			// documentation (case l).
			a = areaOf(p, areas)
		}
		dir, base := path.Split(p)
		name := it.QualifiedID[strings.LastIndexByte(it.QualifiedID, '/')+1:]
		// The words in the symbol's and file's names, and a file named
		// after several of them before the rest: orgtokens.go names "org"
		// and "token" itself.
		c := 16 * termsIn(base+"/"+name, terms)
		if b := termsIn(base, terms); b >= 2 {
			c += b
		}
		covers[it.Read] = max(covers[it.Read], c)
		if a >= 0 && termsIn(path.Base(dir)+"/"+base+"/"+name, terms) >= 2 {
			// Every area the range lies in, not only the closest:
			// gateway/local.go holds "local" for the area "gateway".
			for j := range areas {
				if areaOf(p, areas[j:j+1]) == 0 {
					held[j] = true
				}
			}
		}
		switch {
		case s && it.Retrieval == "exact" && codeNamed:
			exact = append(exact, it.Read)
		case a >= 0 && s:
			on[a] = append(on[a], it.Read)
		case a >= 0 && !docs:
			// The code behind a page the prompt names; for the documentation
			// it is only code that shares a name with it.
			off[a] = append(off[a], it.Read)
		case s:
			rest = append(rest, it.Read)
		case (ui || docs) && it.Kind == model.KindType && it.Retrieval != "exact":
			// A type off the prompt's side that only shares a word with it.
		default:
			other = append(other, it.Read)
		}
	}
	for i, a := range areas {
		modules, err := areaModules(ctx, r, repo, a)
		if err != nil {
			return BriefPlan{}, err
		}
		handlers, err := areaHandlers(ctx, r, repo, a)
		if err != nil {
			return BriefPlan{}, err
		}
		if !held[i] && len(modules) == 0 {
			// Nothing minimal_context found there holds another word of the
			// prompt, and it is no directory of pages: the word named a part
			// of the code base the prompt is not about ("list", "default",
			// "access" in "my token can access private repos", case l).
			on[i], off[i] = nil, nil
			continue
		}
		var onMods, offMods []string
		for _, f := range modules {
			if side(f) {
				onMods = append(onMods, f)
			} else {
				offMods = append(offMods, f)
			}
		}
		if docs {
			offMods, handlers = nil, nil
		}
		on[i] = append(on[i], onMods...)
		off[i] = slices.Concat(off[i], offMods)
		// The handlers of the area's routes follow its best match, on either
		// side: the server side of a page or feature does not share its words.
		q := slices.Concat(on[i], off[i])
		n := min(len(q), 1)
		on[i], off[i] = slices.Concat(q[:n], handlers, q[n:]), nil
	}

	plan := BriefPlan{Ranges: []string{}, Keywords: mc.Keywords}
	perFile := map[string]int{}
	take := func(loc string) bool {
		p := rangePath(loc)
		if len(plan.Ranges) == maxBriefRanges || slices.Contains(plan.Ranges, loc) || perFile[p] == maxRangesPerFile {
			return false
		}
		perFile[p]++
		plan.Ranges = append(plan.Ranges, loc)
		return true
	}
	for _, loc := range slices.Concat(named, exact) {
		take(loc)
	}
	// One range from each area in turn, so the first area named cannot crowd
	// out the others.
	queues := make([][]string, len(areas))
	for i := range areas {
		queues[i] = slices.Concat(on[i], off[i])
	}
	for more := true; more && len(plan.Ranges) < maxBriefRanges; {
		more = false
		for i := range queues {
			for len(queues[i]) > 0 {
				loc := queues[i][0]
				queues[i] = queues[i][1:]
				if take(loc) {
					break
				}
			}
			more = more || len(queues[i]) > 0
		}
	}
	if !codeNamed {
		// A prompt in plain words: a range that holds several of them
		// (orgtokens.go for "org" and "token") says more than one that
		// shares a single common word with it (vuln.key, case l).
		byCover := func(x, y string) int { return cmp.Compare(covers[y], covers[x]) }
		slices.SortStableFunc(rest, byCover)
		slices.SortStableFunc(other, byCover)
	}
	for _, loc := range rest {
		take(loc)
	}
	kept := 0
	for _, loc := range other {
		if (ui || docs) && found && kept == maxBackEndRanges {
			break
		}
		if take(loc) {
			kept++
		}
	}
	return plan, nil
}

// promptTerms are the plain words of a prompt that may name something,
// lower-cased: at least three letters and not a stopword.
func promptTerms(prompt string) []string {
	var out []string
	for _, w := range areaWord.FindAllString(prompt, -1) {
		w = strings.ToLower(strings.Trim(w, "_"))
		if len(w) >= 3 && !agentctx.Stopword(w) && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// termsIn counts the terms a range's names hold: "org" and "token" in
// orgtokens.go, "llm" and "queue" in llm/queue.go. The keywords an item of
// minimal_context matched are not counted: a search also matches the module
// path ("github") of every Go name.
func termsIn(names string, terms []string) int {
	where := strings.ToLower(names)
	n := 0
	for _, t := range terms {
		if strings.Contains(where, t) {
			n++
		}
	}
	return n
}

// rangePath is the file of a "path:start-end" location.
func rangePath(loc string) string {
	if i := strings.LastIndexByte(loc, ':'); i > 0 {
		return loc[:i]
	}
	return loc
}

// pathWord is a word of a prompt that may be a repository path.
var pathWord = regexp.MustCompile(`[A-Za-z0-9_.\-/\[\]]+`)

// namedFiles are the indexed files a prompt names by their repository path,
// as whole-file or whole-configuration ranges.
func namedFiles(ctx context.Context, r store.Reader, repo, prompt string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, w := range pathWord.FindAllString(prompt, -1) {
		w = strings.TrimPrefix(strings.TrimRight(w, ".-"), "./")
		if seen[w] || path.Ext(w) == "" || strings.Contains(w, "..") || strings.HasPrefix(w, "/") {
			continue
		}
		seen[w] = true
		if len(seen) > maxNamedPaths {
			break
		}
		for _, kind := range []model.NodeKind{model.KindFile, model.KindConfig, model.KindTemplate, model.KindDocument} {
			n, err := r.Node(ctx, model.NodeID(repo, kind, w))
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if loc := n.Location(); loc != "" {
				out = append(out, loc)
			}
			break
		}
	}
	return out, nil
}

// areaWord is a plain word of a prompt that may name a package or a file;
// "llm-config" is two of them.
var areaWord = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]+`)

// verbLead are words after which a prompt's next word is a verb ("can
// access", "should route"): what is done, not a part of the code base.
var verbLead = map[string]bool{
	"can": true, "could": true, "will": true, "would": true, "should": true, "must": true, "may": true,
	"might": true, "to": true, "cannot": true, "don": true, "doesn": true, "didn": true, "won": true,
}

// promptAreas are the plain words of a prompt that name a package of the
// repository (a Go package, a page or component of the front end, a Python
// module) or the base name of a file (queue.go), lower-cased, in the order
// the prompt names them. Stopwords and verbs ("can access") name no area,
// nor does the documentation ("docs") unless the prompt is about it: "when
// I write docs / code" is not about internal/ingest/docs (case l).
func promptAreas(ctx context.Context, r store.Reader, repo, prompt string, docs bool) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	prev := ""
	for _, w := range areaWord.FindAllString(prompt, -1) {
		w = strings.ToLower(strings.Trim(w, "_"))
		verb := verbLead[prev]
		prev = w
		if len(w) < 3 || seen[w] || verb || agentctx.Stopword(w) || !docs && asklocal.AboutDocs(w) {
			continue
		}
		seen[w] = true
		if len(seen) > maxAreaWords || len(out) == maxAreas {
			break
		}
		hits, err := r.Search(ctx, repo, w, []model.NodeKind{model.KindPackage, model.KindFile}, 20)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(hits, func(n model.Node) bool { return !n.IsTest && areaName(n) == w }) {
			out = append(out, w)
		}
	}
	return out, nil
}

// areaName is what a package or file is called in a prompt: the last element
// of a slashed or dotted package name, or a file's base name without its
// extension, lower-cased.
func areaName(n model.Node) string {
	name := n.Name
	if n.Kind == model.KindFile {
		name = strings.TrimSuffix(path.Base(n.File), path.Ext(n.File))
	} else if i := strings.LastIndexAny(name, "/."); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(name)
}

// areaOf is the index of the area a repository path lies in: a directory or
// the file's own name (gateway.go, stats.vue) is the area word. The closest
// one wins, so llm/queue.go is in "queue" when the prompt names both. It is
// -1 for none.
func areaOf(p string, areas []string) int {
	if len(areas) == 0 {
		return -1
	}
	segs := strings.Split(strings.ToLower(p), "/")
	last := len(segs) - 1
	segs[last] = strings.TrimSuffix(segs[last], path.Ext(segs[last]))
	for j := last; j >= 0; j-- {
		if i := slices.Index(areas, segs[j]); i >= 0 {
			return i
		}
	}
	return -1
}

// areaModules are the files of an area directory that the graph indexes as
// modules of their own (the pages under pages/stats/ for "stats"), whole, at
// most maxAreaModules. A Go package of that name is not one: minimal_context
// already ranks its symbols, and its files are large.
func areaModules(ctx context.Context, r store.Reader, repo, area string) ([]string, error) {
	pkgs, err := r.Search(ctx, repo, area, []model.NodeKind{model.KindPackage}, 60)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(pkgs, func(x, y model.Node) int { return cmp.Compare(x.QualifiedName, y.QualifiedName) })
	var out []string
	for _, pkg := range pkgs {
		segs := strings.Split(strings.ToLower(pkg.QualifiedName), "/")
		if len(out) == maxAreaModules || !slices.Contains(segs[:len(segs)-1], area) {
			continue
		}
		files, err := r.Traverse(ctx, pkg.ID, []model.EdgeKind{model.EdgeContains}, store.Out, 1, 4)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.Node.Kind == model.KindFile && !f.Node.IsTest && f.Node.File != "" && len(out) < maxAreaModules {
				out = append(out, f.Node.File)
			}
		}
	}
	return out, nil
}

// areaHandlers are the handlers of the API routes whose resource is an area
// (GET /api/v1/stats/series for "stats"), shortest paths first, at most
// maxAreaRoutes: the server side of a page or feature the prompt names. A
// route where the word is only an action or a sub-path
// (POST /api/v1/imports/list) belongs to another resource.
func areaHandlers(ctx context.Context, r store.Reader, repo, area string) ([]string, error) {
	routes, err := r.Search(ctx, repo, area, []model.NodeKind{model.KindRoute}, 60)
	if err != nil {
		return nil, err
	}
	routes = slices.DeleteFunc(routes, func(n model.Node) bool { return !slices.Contains(routeResources(n.Name), area) })
	slices.SortFunc(routes, func(x, y model.Node) int {
		return cmp.Or(cmp.Compare(len(x.Name), len(y.Name)), cmp.Compare(x.Name, y.Name))
	})
	var out []string
	for _, route := range routes[:min(len(routes), maxAreaRoutes)] {
		hs, err := r.Traverse(ctx, route.ID, []model.EdgeKind{model.EdgeHandledBy}, store.Out, 1, 2)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			if loc := h.Node.Location(); loc != "" {
				out = append(out, loc)
			}
		}
	}
	return out, nil
}

// apiVersion is a version segment of an API path.
var apiVersion = regexp.MustCompile(`^v[0-9]+$`)

// routeResources are the resources of a route's path, lower-cased: its first
// segment after any "api" and version prefix, and each segment that follows
// a path parameter (a nested collection): "stats" for
// GET /api/v1/stats/series, "accounts" and "gateway" for
// GET /api/v1/accounts/orgs/{org}/gateway.
func routeResources(route string) []string {
	if _, p, ok := strings.Cut(route, " "); ok {
		route = p
	}
	var out []string
	param := true
	for seg := range strings.SplitSeq(strings.ToLower(route), "/") {
		switch {
		case seg == "" || seg == "api" || apiVersion.MatchString(seg):
		case strings.HasPrefix(seg, "{"):
			param = true
		default:
			if param {
				out = append(out, seg)
			}
			param = false
		}
	}
	return out
}
