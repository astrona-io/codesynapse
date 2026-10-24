package golang

import (
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Calls into first-party modules (docs/PLAN.md, "Cross-repository impact"):
// External nodes are otherwise kept only for sensitive APIs, which keeps the
// graph small (storing every third-party call grew it by up to a third of
// its nodes and two thirds of its edges). Calls into the organisation's own
// modules are few and are what another repository's change can break, so
// they are kept: get_pr_context on a library finds its callers in the
// repositories that use it by the External node's ID.

// codeHosts are hosts whose second path element is an owner (user or
// organisation). Anywhere else the host alone is the organisation, as for
// vanity import paths.
var codeHosts = map[string]bool{"github.com": true, "gitlab.com": true, "bitbucket.org": true, "codeberg.org": true}

// FirstPartyPrefix is the import path prefix of modules from the same owner
// as module: "github.com/acme" for "github.com/acme/app", "go.acme.dev" for
// "go.acme.dev/app". It is "" when module has no host to go by.
func FirstPartyPrefix(module string) string {
	parts := strings.Split(module, "/")
	if !strings.Contains(parts[0], ".") {
		return ""
	}
	if codeHosts[parts[0]] {
		if len(parts) < 2 {
			return ""
		}
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// LinkFirstParty adds an External node and CALLS edges for every called
// function of a first-party module that is not already a node (sensitive
// APIs are linked during extraction).
func LinkFirstParty(repo string, g *model.Graph, res *Result, module string) {
	prefix := FirstPartyPrefix(module)
	if prefix == "" || res == nil {
		return
	}
	var names []string
	for name, pkg := range res.ExternalPackages {
		if pkg == prefix || strings.HasPrefix(pkg, prefix+"/") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		id := model.NodeID(repo, model.KindExternal, name)
		if _, ok := g.Node(id); ok {
			continue
		}
		g.AddNode(model.Node{ID: id, Kind: model.KindExternal, Repo: repo, Name: name[strings.LastIndex(name, ".")+1:], QualifiedName: name, Package: res.ExternalPackages[name]})
		callers := slices.Clone(res.ExternalCalls[name])
		slices.Sort(callers)
		for i := 0; i < len(callers); {
			j := i
			for j < len(callers) && callers[j] == callers[i] {
				j++
			}
			if _, ok := g.Node(callers[i]); ok {
				g.AddEdge(model.Edge{From: callers[i], To: id, Kind: model.EdgeCalls, Evidence: model.EvidenceTyped, Count: j - i})
			}
			i = j
		}
	}
}
