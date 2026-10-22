// Package docs indexes Markdown documentation as Document and Section nodes.
//
// Each heading becomes a Section with its line range and a short redacted
// summary, so an agent can find the one section it needs and read only those
// lines. Inline code spans that name a known code symbol become MENTIONS
// edges, which tie documentation to the code it describes.
package docs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

const (
	summaryLen = 240
	// maxDocSize skips generated or vendored Markdown blobs.
	maxDocSize = 2 << 20
)

// skipDirs are never walked.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".nuxt": true,
	".output": true, "dist": true, "testdata": true, ".idea": true, ".vscode": true,
}

var (
	headingRe  = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
	codeSpanRe = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_./*()]*)`")
)

// Extract walks root for Markdown files and returns their graph. symbols is
// the code graph already extracted for the same repo; it is used to resolve
// MENTIONS edges and may be nil. MENTIONS edges point at nodes in symbols, so
// the result must be merged into the code graph before it is persisted.
func Extract(ctx context.Context, repo, root string, symbols *model.Graph) (*model.Graph, error) {
	g := model.NewGraph()
	repoID := model.NodeID(repo, model.KindRepository, repo)
	g.AddNode(model.Node{ID: repoID, Kind: model.KindRepository, Repo: repo, Name: repo})
	index := symbolIndex(symbols)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			// Nested repositories (their own .git) are separate graphs.
			if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") || isRepo(path) {
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks could point outside the repository; never follow them.
		if d.Type()&fs.ModeSymlink != 0 || !isMarkdown(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxDocSize {
			return nil //nolint:nilerr // unreadable or oversized docs are skipped, not fatal
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path) //nolint:gosec // path is inside root and not a symlink
		if err != nil {
			return fmt.Errorf("reading %s: %w", rel, err)
		}
		addDocument(g, repo, repoID, filepath.ToSlash(rel), content, index)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking docs: %w", err)
	}
	return g, nil
}

func isRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

func isMarkdown(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".markdown" || ext == ".mdx"
}

// symbolIndex maps short and qualified names of exported code symbols to IDs.
// Ambiguous short names map to nothing to avoid false MENTIONS edges.
func symbolIndex(g *model.Graph) map[string]string {
	idx := map[string]string{}
	if g == nil {
		return idx
	}
	ambiguous := map[string]bool{}
	for _, n := range g.Nodes() {
		switch n.Kind {
		case model.KindFunction, model.KindMethod, model.KindType, model.KindInterfaceMethod:
		default:
			continue
		}
		if !n.Exported {
			continue
		}
		idx[n.QualifiedName] = n.ID
		short := n.Name
		if n.Kind == model.KindMethod || n.Kind == model.KindInterfaceMethod {
			short = receiverShort(n.QualifiedName) + "." + n.Name
		}
		if prev, ok := idx[short]; ok && prev != n.ID {
			ambiguous[short] = true
		}
		idx[short] = n.ID
	}
	for k := range ambiguous {
		delete(idx, k)
	}
	return idx
}

// receiverShort turns "(*example.com/p.T).M" into "T".
func receiverShort(qualified string) string {
	recv, _, ok := strings.Cut(strings.TrimPrefix(qualified, "("), ")")
	if !ok {
		return ""
	}
	recv = strings.TrimPrefix(recv, "*")
	if i := strings.LastIndexByte(recv, '.'); i >= 0 {
		return recv[i+1:]
	}
	return recv
}

type section struct {
	title    string
	level    int
	start    int
	end      int
	body     strings.Builder
	mentions []string
}

func addDocument(g *model.Graph, repo, repoID, rel string, content []byte, index map[string]string) {
	docID := model.NodeID(repo, model.KindDocument, rel)
	var (
		sections []*section
		cur      = &section{title: "(preamble)", level: 0, start: 1}
		inFence  bool
		lineNo   int
	)
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), maxDocSize)
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			if m := headingRe.FindStringSubmatch(line); m != nil {
				cur.end = lineNo - 1
				sections = append(sections, cur)
				cur = &section{title: strings.TrimSpace(m[2]), level: len(m[1]), start: lineNo}
				continue
			}
			for _, m := range codeSpanRe.FindAllStringSubmatch(line, -1) {
				cur.mentions = append(cur.mentions, m[1])
			}
			if cur.body.Len() < summaryLen*2 {
				cur.body.WriteString(line)
				cur.body.WriteByte(' ')
			}
		}
	}
	cur.end = lineNo
	sections = append(sections, cur)

	title := rel
	for _, s := range sections {
		if s.level > 0 {
			title = s.title
			break
		}
	}
	g.AddNode(model.Node{
		ID: docID, Kind: model.KindDocument, Repo: repo, Name: title, File: rel,
		StartLine: 1, EndLine: lineNo, LOC: lineNo, Bytes: len(content),
		Summary: redact.Summary(firstBody(sections), summaryLen),
	})
	g.AddEdge(model.Edge{From: repoID, To: docID, Kind: model.EdgeContains})

	for _, s := range sections {
		body := strings.TrimSpace(s.body.String())
		if s.level == 0 && body == "" {
			continue
		}
		secID := model.NodeID(repo, model.KindSection, fmt.Sprintf("%s#L%d", rel, s.start))
		g.AddNode(model.Node{
			ID: secID, Kind: model.KindSection, Repo: repo, Name: redact.Summary(s.title, 120),
			File: rel, StartLine: s.start, EndLine: max(s.start, s.end), Level: s.level,
			LOC: max(s.start, s.end) - s.start + 1, Summary: redact.Summary(body, summaryLen),
		})
		g.AddEdge(model.Edge{From: docID, To: secID, Kind: model.EdgeContains})
		for _, name := range s.mentions {
			if target, ok := index[strings.TrimSuffix(name, "()")]; ok {
				g.AddEdge(model.Edge{From: secID, To: target, Kind: model.EdgeMentions})
			}
		}
	}
}

func firstBody(sections []*section) string {
	for _, s := range sections {
		if b := strings.TrimSpace(s.body.String()); b != "" {
			return b
		}
	}
	return ""
}
