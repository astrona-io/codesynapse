package config

import (
	"bytes"
	"strings"
	"text/template/parse"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/redact"
)

// templateFile indexes a Go template. chart is the Helm chart root ("" when
// the file is not part of a chart); values references resolve against that
// chart's values.yaml entries.
func (b *builder) templateFile(rel string, content []byte, chart string, refs *templateRefs) {
	lines := bytes.Count(content, []byte("\n")) + 1
	fileID := b.id(model.KindTemplate, rel)
	file := b.g.AddNode(model.Node{
		ID: fileID, Kind: model.KindTemplate, Repo: b.repo, Name: baseName(rel), File: rel,
		StartLine: 1, EndLine: lines, LOC: lines, Bytes: len(content), Package: chart,
	})
	b.g.AddEdge(model.Edge{From: model.NodeID(b.repo, model.KindRepository, b.repo), To: fileID, Kind: model.EdgeContains})

	// Templates that render Kubernetes manifests get the YAML security
	// rules on their literal values; Language "yaml" marks them as scanned
	// for the security report's coverage.
	if chart != "" && b.scanTemplate(rel, fileID, content) {
		file.Language = "yaml"
	}
	trees, err := parseTemplate(rel, string(content))
	if err != nil {
		file.Summary = "unparseable template: " + redact.Summary(err.Error(), 120)
		return
	}
	lineOf := func(pos parse.Pos) int { return bytes.Count(content[:min(int(pos), len(content))], []byte("\n")) + 1 }

	var defs []string
	for name, tree := range trees {
		if tree.Root == nil {
			continue
		}
		ownerID := fileID
		if name != rel {
			// A {{define}} block. Helm shares define names across a chart.
			defID := b.id(model.KindTemplateDef, chartScope(chart)+name)
			start := lineOf(tree.Root.Position())
			b.g.AddNode(model.Node{
				ID: defID, Kind: model.KindTemplateDef, Repo: b.repo, Name: name, QualifiedName: name,
				File: rel, StartLine: max(1, start-1), EndLine: start + strings.Count(tree.Root.String(), "\n"), Package: chart,
			})
			b.g.AddEdge(model.Edge{From: fileID, To: defID, Kind: model.EdgeDefines})
			ownerID = defID
			defs = append(defs, name)
		}
		visit(tree.Root, func(n parse.Node) {
			switch n := n.(type) {
			case *parse.TemplateNode:
				refs.includes = append(refs.includes, templateRef{from: ownerID, chart: chart, name: n.Name})
			case *parse.CommandNode:
				// Helm: include "name" . / template "name" .
				if len(n.Args) >= 2 {
					if id, ok := n.Args[0].(*parse.IdentifierNode); ok && (id.Ident == "include" || id.Ident == "template") {
						if s, ok := n.Args[1].(*parse.StringNode); ok {
							refs.includes = append(refs.includes, templateRef{from: ownerID, chart: chart, name: s.Text})
						}
					}
				}
			case *parse.FieldNode:
				if len(n.Ident) >= 2 && n.Ident[0] == "Values" {
					refs.values = append(refs.values, templateRef{from: ownerID, chart: chart, name: strings.Join(n.Ident[1:], ".")})
				}
			}
		})
	}
	if len(defs) > 0 {
		file.Summary = redact.Summary("defines "+strings.Join(defs, ", "), 240)
	}
}

func chartScope(chart string) string {
	if chart == "" {
		return ""
	}
	return chart + ":"
}

// parseTemplate parses text without knowing its function map (Helm and
// sprig functions are unknown here), returning the file tree plus one tree
// per {{define}}.
func parseTemplate(name, text string) (map[string]*parse.Tree, error) {
	t := parse.New(name)
	t.Mode = parse.SkipFuncCheck | parse.ParseComments
	treeSet := map[string]*parse.Tree{}
	if _, err := t.Parse(text, "{{", "}}", treeSet); err != nil {
		return nil, err
	}
	return treeSet, nil
}

// visit walks every node of a template tree.
func visit(n parse.Node, fn func(parse.Node)) {
	if n == nil {
		return
	}
	fn(n)
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			visit(c, fn)
		}
	case *parse.ActionNode:
		visit(n.Pipe, fn)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, c := range n.Cmds {
			visit(c, fn)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			visit(a, fn)
		}
	case *parse.IfNode:
		visit(n.Pipe, fn)
		visit(n.List, fn)
		visit(n.ElseList, fn)
	case *parse.RangeNode:
		visit(n.Pipe, fn)
		visit(n.List, fn)
		visit(n.ElseList, fn)
	case *parse.WithNode:
		visit(n.Pipe, fn)
		visit(n.List, fn)
		visit(n.ElseList, fn)
	case *parse.TemplateNode:
		visit(n.Pipe, fn)
	case *parse.ChainNode:
		visit(n.Node, fn)
	}
}

type templateRef struct {
	from, chart, name string
}

// templateRefs collects references that can only be resolved after every
// file of the repository has been indexed.
type templateRefs struct {
	includes []templateRef
	values   []templateRef
}

// resolve links includes to define blocks and .Values paths to the longest
// matching values.yaml entry of the same chart.
func (b *builder) resolve(refs *templateRefs, valuesFiles map[string]string) {
	for _, r := range refs.includes {
		id := b.id(model.KindTemplateDef, chartScope(r.chart)+r.name)
		if _, ok := b.g.Node(id); ok {
			b.g.AddEdge(model.Edge{From: r.from, To: id, Kind: model.EdgeIncludes})
		}
	}
	for _, r := range refs.values {
		vf, ok := valuesFiles[r.chart]
		if !ok {
			continue
		}
		parts := strings.Split(r.name, ".")
		for i := len(parts); i > 0; i-- {
			id := b.id(model.KindConfigEntry, vf+"#"+strings.Join(parts[:i], "."))
			if _, ok := b.g.Node(id); ok {
				b.g.AddEdge(model.Edge{From: r.from, To: id, Kind: model.EdgeUsesValue})
				break
			}
		}
	}
}
