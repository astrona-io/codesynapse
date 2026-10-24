package agentctx

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/astrona-io/codesynapse/internal/codeowners"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
)

// maxChangeOwners bounds the owners a change lists.
const maxChangeOwners = 20

// changeOwners are the CODEOWNERS owners of the changed files, as
// "@org/team (3 files)", the owner of most files first. A repository
// without a CODEOWNERS file, or a change no rule covers, gives none.
func (s Service) changeOwners(ctx context.Context, repo string, changes []gitx.FileChange) []string {
	n, err := s.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil
	}
	set := codeowners.Compile(model.CodeOwnersOf(n))
	if set.Empty() {
		return nil
	}
	files := map[string]int{}
	for _, c := range changes {
		for _, o := range set.Owners(c.Path) {
			files[o]++
		}
	}
	owners := make([]string, 0, len(files))
	for o := range files {
		owners = append(owners, o)
	}
	slices.SortFunc(owners, func(a, b string) int { return cmp.Or(cmp.Compare(files[b], files[a]), cmp.Compare(a, b)) })
	out := make([]string, 0, min(len(owners), maxChangeOwners))
	for _, o := range owners[:min(len(owners), maxChangeOwners)] {
		word := "files"
		if files[o] == 1 {
			word = "file"
		}
		out = append(out, fmt.Sprintf("%s (%d %s)", o, files[o], word))
	}
	return out
}
