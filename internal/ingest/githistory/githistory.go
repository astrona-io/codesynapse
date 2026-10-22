// Package githistory indexes recent commits, their authors and the files they
// touched. Churn per file feeds hotspot ranking (complexity x change rate),
// and MODIFIES edges answer "what changed here recently" without git access.
package githistory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// Extract reads up to limit commits from the repository at root and adds
// Commit and Author nodes to g. MODIFIES edges are only created for File
// nodes already in g, and those files get their Churn set.
func Extract(ctx context.Context, repo, root string, limit int, g *model.Graph) error {
	commits, err := gitx.Log(ctx, root, limit)
	if err != nil {
		return err
	}
	Apply(repo, commits, g)
	return nil
}

// Apply adds commits to g. It is separate from Extract so it can be tested
// without a git repository.
func Apply(repo string, commits []gitx.Commit, g *model.Graph) {
	repoID := model.NodeID(repo, model.KindRepository, repo)
	for _, c := range commits {
		commitID := model.NodeID(repo, model.KindCommit, c.Hash)
		short := c.Hash
		if len(short) > 12 {
			short = short[:12]
		}
		g.AddNode(model.Node{
			ID: commitID, Kind: model.KindCommit, Repo: repo, Name: short,
			QualifiedName: c.Hash, Summary: redact.Summary(c.Subject, 160), Time: c.Time,
		})
		g.AddEdge(model.Edge{From: repoID, To: commitID, Kind: model.EdgeContains})

		// Authors are keyed by a hash of the e-mail so the graph does not hold
		// addresses; only the display name is stored.
		authorID := model.NodeID(repo, model.KindAuthor, emailKey(c.AuthorEmail))
		g.AddNode(model.Node{ID: authorID, Kind: model.KindAuthor, Repo: repo, Name: c.AuthorName})
		g.AddEdge(model.Edge{From: authorID, To: commitID, Kind: model.EdgeAuthored})

		for _, f := range c.Files {
			fileID := model.NodeID(repo, model.KindFile, f.Path)
			n, ok := g.Node(fileID)
			if !ok {
				continue
			}
			n.Churn++
			g.AddEdge(model.Edge{From: commitID, To: fileID, Kind: model.EdgeModifies, Count: max(1, f.Added+f.Removed)})
		}
	}
}

func emailKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:8])
}
