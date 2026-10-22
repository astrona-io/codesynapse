package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
)

type duplicateCodeIn struct {
	RepoArg
	Scope string `json:"scope,omitempty" jsonschema:"only copies this far apart: file, package or repository (default all)"`
	Kind  string `json:"kind,omitempty" jsonschema:"exact (same structure) or near (edited copies); default both"`
	Limit int    `json:"limit,omitempty" jsonschema:"max groups, default 10"`
}

func (h *handlers) duplicateCode(ctx context.Context, _ *mcp.CallToolRequest, in duplicateCodeIn) (*mcp.CallToolResult, duplicates.Report, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, duplicates.Report{}, err
	}
	n, err := h.svc.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil, duplicates.Report{}, err
	}
	if n.Duplicates == "" {
		return nil, duplicates.Report{}, errors.New("no duplicate report stored; re-ingest the repository")
	}
	rep, err := duplicates.Decode(n.Duplicates)
	if err != nil {
		return nil, rep, err
	}
	rep.Fingerprints = nil
	groups := rep.Groups[:0:0]
	for _, g := range rep.Groups {
		if (in.Scope == "" || in.Scope == "all" || in.Scope == g.Scope) && (in.Kind == "" || in.Kind == g.Kind) {
			groups = append(groups, g)
		}
	}
	rep.Groups = groups[:min(len(groups), clampInt(in.Limit, 10, 50))]
	return nil, rep, nil
}

type sharedCodeIn struct {
	RepoPrefix string `json:"repo_prefix,omitempty" jsonschema:"only repositories whose name starts with this, e.g. astrona-io/"`
	Kind       string `json:"kind,omitempty" jsonschema:"exact (same structure) or near (edited copies); default both"`
	Limit      int    `json:"limit,omitempty" jsonschema:"max groups, default 10"`
}

func (h *handlers) sharedCode(ctx context.Context, _ *mcp.CallToolRequest, in sharedCodeIn) (*mcp.CallToolResult, duplicates.CrossReport, error) {
	repos, err := h.visibleRepos(ctx)
	if err != nil {
		return nil, duplicates.CrossReport{}, err
	}
	reports := map[string]duplicates.Report{}
	for _, n := range repos {
		if n.Duplicates == "" || !strings.HasPrefix(n.Name, in.RepoPrefix) {
			continue
		}
		rep, err := duplicates.Decode(n.Duplicates)
		if err != nil {
			return nil, duplicates.CrossReport{}, fmt.Errorf("%s: %w", n.Name, err)
		}
		reports[n.Name] = rep
	}
	out := duplicates.Across(reports)
	if in.Kind != "" {
		out.Groups = slices.DeleteFunc(out.Groups, func(g duplicates.CrossGroup) bool { return g.Kind != in.Kind })
	}
	out.Groups = out.Groups[:min(len(out.Groups), clampInt(in.Limit, 10, 50))]
	return nil, out, nil
}
