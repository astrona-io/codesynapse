package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/security"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// staleAfter matches the dashboard: an index older than a week is stale.
const staleAfter = 7 * 24 * time.Hour

func registerDiscoveryTools(s *mcp.Server, h *handlers, add func(name, desc string) *mcp.Tool) {
	mcp.AddTool(s, add("repo_status",
		"How fresh the graph of a repository is: indexed branch, commit and time, whether the index is stale, and whether the checkout on disk has moved on since (newer commit or uncommitted changes). Call it when answers look out of date."),
		h.repoStatus)
	mcp.AddTool(s, add("search_all_repos",
		"Search every indexed repository at once (for example a whole GitHub organisation) by name substring. Returns a few hits per repository with their repo, so you can find which repository owns something before opening it. repo_prefix narrows it, e.g. astrona-io/."),
		h.searchAll)
	mcp.AddTool(s, add("get_config_entries",
		"Kubernetes, Helm, compose, CI and other YAML entries (key paths, resources) and Go template definitions, by file prefix and/or name substring, with line ranges. Cheaper than reading the YAML."),
		h.configEntries)
	mcp.AddTool(s, add("get_doc_section",
		"Markdown sections by heading text and/or file, with the code symbols each section mentions and line ranges to read. Section text is not stored: read the returned lines for the body."),
		h.docSection)
}

// --- repo_status ------------------------------------------------------------

type repoStatusOut struct {
	Repo       string  `json:"repo"`
	Branch     string  `json:"branch,omitempty"`
	Head       string  `json:"head,omitempty"`
	IngestedAt string  `json:"ingested_at,omitempty"`
	AgeHours   float64 `json:"age_hours"`
	Stale      bool    `json:"stale"`
	Remote     string  `json:"remote,omitempty"`
	// CheckoutHead is the commit checked out on disk now, when the server can
	// read the checkout. CheckoutChanged is true when it differs from Head
	// or the tree has uncommitted changes.
	CheckoutHead    string `json:"checkout_head,omitempty"`
	CheckoutChanged *bool  `json:"checkout_changed,omitempty"`
	Dirty           *bool  `json:"dirty,omitempty"`
	Advice          string `json:"advice"`
}

func (h *handlers) repoStatus(ctx context.Context, _ *mcp.CallToolRequest, in RepoArg) (*mcp.CallToolResult, repoStatusOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, repoStatusOut{}, err
	}
	n, err := h.svc.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil, repoStatusOut{}, err
	}
	out := repoStatusOut{Repo: repo, Branch: n.Branch, Head: n.Head, IngestedAt: n.Time, Remote: gitx.SanitizeURL(n.URL)}
	if t, err := time.Parse(time.RFC3339, n.Time); err == nil {
		age := time.Since(t)
		out.AgeHours = float64(int(age.Hours()*10)) / 10
		out.Stale = age > staleAfter
	}
	// The checkout is only readable when this server shares the filesystem
	// with the API (compose, or the shared workspace volume).
	if filepath.IsAbs(n.Root) {
		if st, err := os.Stat(n.Root); err == nil && st.IsDir() {
			if head := gitx.HeadCommit(ctx, n.Root); head != "" {
				dirty := gitx.IsDirty(ctx, n.Root)
				changed := head != n.Head || dirty
				out.CheckoutHead, out.Dirty, out.CheckoutChanged = head, &dirty, &changed
			}
		}
	}
	switch {
	case out.CheckoutChanged != nil && *out.CheckoutChanged && out.CheckoutHead != out.Head:
		out.Advice = "The checkout has a different commit than the graph: answers may miss newer commits. Sync the repository (dashboard Sync button, or wait for auto-sync), and pass a diff to get_pr_context for current changes."
	case out.Dirty != nil && *out.Dirty:
		out.Advice = "The checkout has uncommitted changes the graph does not know. Pass your diff to get_pr_context."
	case out.Stale:
		out.Advice = "The index is more than a week old. Sync the repository before relying on it."
	default:
		out.Advice = "The graph is current as far as this server can tell."
	}
	return nil, out, nil
}

// --- search_all_repos -------------------------------------------------------

const (
	searchAllDefaultLimit = 30
	searchAllMaxLimit     = 100
	searchAllPerRepoMax   = 20
)

type searchAllIn struct {
	Query      string   `json:"query" jsonschema:"case-insensitive name substring"`
	Kinds      []string `json:"kinds,omitempty" jsonschema:"filter by node kind, as in search"`
	RepoPrefix string   `json:"repo_prefix,omitempty" jsonschema:"only repositories whose name starts with this, e.g. astrona-io/"`
	PerRepo    int      `json:"per_repo,omitempty" jsonschema:"max hits per repository, default 3"`
	Limit      int      `json:"limit,omitempty" jsonschema:"max hits in total, default 30, max 100"`
}

type repoHit struct {
	Repo string `json:"repo"`
	query.Ref
}

type searchAllOut struct {
	Results       []repoHit `json:"results"`
	ReposSearched int       `json:"repos_searched"`
	// Truncated is true when the limit stopped the search before every
	// repository was searched; narrow the query or set repo_prefix.
	Truncated bool `json:"truncated"`
}

func (h *handlers) searchAll(ctx context.Context, _ *mcp.CallToolRequest, in searchAllIn) (*mcp.CallToolResult, searchAllOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, searchAllOut{}, errors.New("query is required")
	}
	kinds := make([]model.NodeKind, 0, len(in.Kinds))
	for _, k := range in.Kinds {
		nk := model.NodeKind(k)
		if !nk.Valid() {
			return nil, searchAllOut{}, fmt.Errorf("unknown kind %q", k)
		}
		kinds = append(kinds, nk)
	}
	limit := clampInt(in.Limit, searchAllDefaultLimit, searchAllMaxLimit)
	perRepo := clampInt(in.PerRepo, 3, searchAllPerRepoMax)
	repos, err := h.visibleRepos(ctx)
	if err != nil {
		return nil, searchAllOut{}, err
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		if strings.HasPrefix(r.Name, in.RepoPrefix) {
			names = append(names, r.Name)
		}
	}
	slices.Sort(names)
	out := searchAllOut{Results: []repoHit{}}
	for _, name := range names {
		if len(out.Results) >= limit {
			out.Truncated = true
			break
		}
		ns, err := h.svc.R.Search(ctx, name, in.Query, kinds, perRepo)
		if err != nil {
			return nil, searchAllOut{}, fmt.Errorf("searching %s: %w", name, err)
		}
		out.ReposSearched++
		for _, n := range ns {
			if len(out.Results) >= limit {
				out.Truncated = true
				break
			}
			out.Results = append(out.Results, repoHit{Repo: name, Ref: query.RefOf(n)})
		}
	}
	return nil, out, nil
}

func clampInt(v, def, hi int) int {
	switch {
	case v <= 0:
		return def
	case v > hi:
		return hi
	}
	return v
}

// --- get_config_entries -----------------------------------------------------

var configKinds = []model.NodeKind{model.KindConfig, model.KindConfigEntry, model.KindTemplate, model.KindTemplateDef}

type configEntriesIn struct {
	RepoArg
	File  string `json:"file,omitempty" jsonschema:"repo-relative file or directory prefix, e.g. deploy/ or charts/api/values.yaml"`
	Query string `json:"query,omitempty" jsonschema:"case-insensitive substring of the entry name (key path, Kind/name for Kubernetes resources)"`
	Limit int    `json:"limit,omitempty" jsonschema:"max entries, default 30, max 100"`
}

type configEntry struct {
	ID        string         `json:"id"`
	Kind      model.NodeKind `json:"kind"`
	Name      string         `json:"name"`
	Location  string         `json:"location"`
	Signature string         `json:"signature,omitempty"`
	Summary   string         `json:"summary,omitempty"`
}

type configEntriesOut struct {
	Entries []configEntry `json:"entries"`
}

func (h *handlers) configEntries(ctx context.Context, _ *mcp.CallToolRequest, in configEntriesIn) (*mcp.CallToolResult, configEntriesOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, configEntriesOut{}, err
	}
	if strings.TrimSpace(in.File) == "" && strings.TrimSpace(in.Query) == "" {
		return nil, configEntriesOut{}, errors.New("pass file, query or both: an unfiltered list of every config entry is rarely useful")
	}
	limit := clampInt(in.Limit, 30, 100)
	prefix := strings.TrimPrefix(strings.TrimSpace(in.File), "./")
	var nodes []model.Node
	if in.Query != "" {
		nodes, err = h.svc.R.Search(ctx, repo, in.Query, configKinds, store.MaxLimit)
		if err != nil {
			return nil, configEntriesOut{}, err
		}
	} else {
		ranked, err := h.svc.R.List(ctx, repo, store.Filter{Kinds: configKinds, FilePrefix: prefix}, store.ByName, store.MaxLimit)
		if err != nil {
			return nil, configEntriesOut{}, err
		}
		for _, r := range ranked {
			nodes = append(nodes, r.Node)
		}
	}
	out := configEntriesOut{Entries: []configEntry{}}
	for _, n := range nodes {
		if !strings.HasPrefix(n.File, prefix) {
			continue
		}
		out.Entries = append(out.Entries, configEntry{ID: n.ID, Kind: n.Kind, Name: n.Name, Location: n.Location(), Signature: n.Signature, Summary: n.Summary})
	}
	slices.SortStableFunc(out.Entries, func(a, b configEntry) int { return strings.Compare(a.Location, b.Location) })
	if len(out.Entries) > limit {
		out.Entries = out.Entries[:limit]
	}
	return nil, out, nil
}

// --- get_doc_section --------------------------------------------------------

type docSectionIn struct {
	RepoArg
	Query string `json:"query,omitempty" jsonschema:"case-insensitive substring of the heading"`
	File  string `json:"file,omitempty" jsonschema:"repo-relative file or directory prefix, e.g. docs/ or README.md"`
	Limit int    `json:"limit,omitempty" jsonschema:"max sections, default 5, max 20"`
}

type docSection struct {
	ID       string      `json:"id"`
	Heading  string      `json:"heading"`
	Level    int         `json:"level,omitempty"`
	Location string      `json:"location"`
	Summary  string      `json:"summary,omitempty"`
	Mentions []query.Ref `json:"mentions,omitempty"`
}

type docSectionOut struct {
	Sections []docSection `json:"sections"`
}

func (h *handlers) docSection(ctx context.Context, _ *mcp.CallToolRequest, in docSectionIn) (*mcp.CallToolResult, docSectionOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, docSectionOut{}, err
	}
	if strings.TrimSpace(in.File) == "" && strings.TrimSpace(in.Query) == "" {
		return nil, docSectionOut{}, errors.New("pass query (heading text), file, or both")
	}
	limit := clampInt(in.Limit, 5, 20)
	prefix := strings.TrimPrefix(strings.TrimSpace(in.File), "./")
	var nodes []model.Node
	if in.Query != "" {
		nodes, err = h.svc.R.Search(ctx, repo, in.Query, []model.NodeKind{model.KindSection}, store.MaxLimit)
		if err != nil {
			return nil, docSectionOut{}, err
		}
	} else {
		ranked, err := h.svc.R.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindSection}, FilePrefix: prefix}, store.ByName, store.MaxLimit)
		if err != nil {
			return nil, docSectionOut{}, err
		}
		for _, r := range ranked {
			nodes = append(nodes, r.Node)
		}
	}
	nodes = slices.DeleteFunc(nodes, func(n model.Node) bool { return !strings.HasPrefix(n.File, prefix) })
	slices.SortStableFunc(nodes, func(a, b model.Node) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		return a.StartLine - b.StartLine
	})
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	out := docSectionOut{Sections: []docSection{}}
	for _, n := range nodes {
		sec := docSection{ID: n.ID, Heading: n.ShortName(), Level: n.Level, Location: n.Location(), Summary: n.Summary}
		hits, err := h.svc.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeMentions}, store.Out, 1, 10)
		if err != nil {
			return nil, docSectionOut{}, err
		}
		for _, hit := range hits {
			sec.Mentions = append(sec.Mentions, query.RefOf(hit.Node))
		}
		out.Sections = append(out.Sections, sec)
	}
	return nil, out, nil
}

// --- security_report path filter -------------------------------------------

// filterFindingsByPath keeps the findings in file or directory path and
// recomputes the counts that describe them. Vulnerabilities belong to the
// dependency graph, not to files, and are left alone.
func filterFindingsByPath(rep *security.Report, path string) {
	path = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(path), "./"), "/")
	if path == "" {
		return
	}
	kept := rep.Findings[:0:0]
	counts := map[string]int{}
	rules := map[string]*security.RuleCount{}
	for _, f := range rep.Findings {
		file, _, _ := strings.Cut(f.Location, ":")
		if file != path && !strings.HasPrefix(file, path+"/") {
			continue
		}
		kept = append(kept, f)
		counts[f.Severity]++
		rc := rules[f.Rule]
		if rc == nil {
			rc = &security.RuleCount{Rule: f.Rule, Title: f.Title, Severity: f.Severity}
			rules[f.Rule] = rc
		}
		rc.Count++
	}
	rep.Findings, rep.FindingCounts = kept, counts
	rep.Rules = rep.Rules[:0:0]
	for _, rc := range rules {
		rep.Rules = append(rep.Rules, *rc)
	}
	slices.SortFunc(rep.Rules, func(a, b security.RuleCount) int {
		if c := model.SeverityRank(b.Severity) - model.SeverityRank(a.Severity); c != 0 {
			return c
		}
		if c := b.Count - a.Count; c != 0 {
			return c
		}
		return strings.Compare(a.Rule, b.Rule)
	})
}
