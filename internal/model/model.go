// Package model defines the property graph that codesynapse extracts from a
// repository: nodes for code, docs and history, and the typed edges between
// them. Every store implementation persists exactly this shape.
package model

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// NodeKind classifies a node. Each kind maps 1:1 to a Neo4j label.
type NodeKind string

// Node kinds.
const (
	KindRepository      NodeKind = "Repository"
	KindPackage         NodeKind = "Package"
	KindFile            NodeKind = "File"
	KindFunction        NodeKind = "Function"
	KindMethod          NodeKind = "Method"
	KindInterfaceMethod NodeKind = "InterfaceMethod"
	KindType            NodeKind = "Type"
	KindExternal        NodeKind = "External"
	KindDocument        NodeKind = "Document"
	KindSection         NodeKind = "Section"
	KindCommit          NodeKind = "Commit"
	KindAuthor          NodeKind = "Author"
	// KindFinding is a security finding from a code rule.
	KindFinding NodeKind = "Finding"
	// KindModule is a dependency from go.mod (or the Go standard library).
	KindModule NodeKind = "Module"
	// KindVulnerability is a known vulnerability (OSV/GHSA/CVE) affecting a Module.
	KindVulnerability NodeKind = "Vulnerability"
	// KindConfig is a YAML file.
	KindConfig NodeKind = "Config"
	// KindConfigEntry is a Kubernetes resource or a key path inside a YAML file.
	KindConfigEntry NodeKind = "ConfigEntry"
	// KindTemplate is a Go template file (including Helm chart templates).
	KindTemplate NodeKind = "Template"
	// KindTemplateDef is a {{define "name"}} block.
	KindTemplateDef NodeKind = "TemplateDef"
)

// AllNodeKinds lists every valid node kind. Stores use it as the allowlist
// for labels, which cannot be passed as Cypher parameters.
var AllNodeKinds = []NodeKind{
	KindRepository, KindPackage, KindFile, KindFunction, KindMethod,
	KindInterfaceMethod, KindType, KindExternal, KindDocument, KindSection,
	KindCommit, KindAuthor, KindFinding, KindModule, KindVulnerability,
	KindConfig, KindConfigEntry, KindTemplate, KindTemplateDef,
}

// Valid reports whether k is a known node kind.
func (k NodeKind) Valid() bool { return slices.Contains(AllNodeKinds, k) }

// IsCallable reports whether nodes of this kind can appear in a call graph.
func (k NodeKind) IsCallable() bool {
	return k == KindFunction || k == KindMethod || k == KindInterfaceMethod || k == KindExternal
}

// EdgeKind classifies an edge. Each kind maps 1:1 to a Neo4j relationship type.
type EdgeKind string

// Edge kinds.
const (
	// EdgeContains links a container to its children:
	// Repository->Package, Repository->Document, Package->File, Document->Section.
	EdgeContains EdgeKind = "CONTAINS"
	// EdgeDefines links a File to the symbols declared in it.
	EdgeDefines EdgeKind = "DEFINES"
	// EdgeHasMethod links a Type to its methods (concrete or interface).
	EdgeHasMethod EdgeKind = "HAS_METHOD"
	// EdgeCalls links a callable to a callable it invokes statically.
	EdgeCalls EdgeKind = "CALLS"
	// EdgeDispatchesTo links an InterfaceMethod to a concrete Method that
	// satisfies it, so callers of the interface reach implementations.
	EdgeDispatchesTo EdgeKind = "DISPATCHES_TO"
	// EdgeImplements links a concrete Type to an interface Type.
	EdgeImplements EdgeKind = "IMPLEMENTS"
	// EdgeImports links a Package to a Package it imports (in-repo only).
	EdgeImports EdgeKind = "IMPORTS"
	// EdgeMentions links a doc Section to a code symbol it references.
	EdgeMentions EdgeKind = "MENTIONS"
	// EdgeModifies links a Commit to a File it changed.
	EdgeModifies EdgeKind = "MODIFIES"
	// EdgeAuthored links an Author to a Commit.
	EdgeAuthored EdgeKind = "AUTHORED"
	// EdgeHasFinding links a function, method or file to a security Finding.
	EdgeHasFinding EdgeKind = "HAS_FINDING"
	// EdgeDependsOn links the Repository to a Module it requires.
	EdgeDependsOn EdgeKind = "DEPENDS_ON"
	// EdgeAffects links a Vulnerability to the Module (and External symbols) it affects.
	EdgeAffects EdgeKind = "AFFECTS"
	// EdgeIncludes links a template (file or define block) to a define block it renders.
	EdgeIncludes EdgeKind = "INCLUDES"
	// EdgeUsesValue links a template to the values.yaml entry it reads (.Values.a.b).
	EdgeUsesValue EdgeKind = "USES_VALUE"
	// EdgeReferences links code to a function or method it uses as a value
	// (a handler, callback or method value) without calling it directly.
	EdgeReferences EdgeKind = "REFERENCES"
)

// AllEdgeKinds lists every valid edge kind (the relationship-type allowlist).
var AllEdgeKinds = []EdgeKind{
	EdgeContains, EdgeDefines, EdgeHasMethod, EdgeCalls, EdgeDispatchesTo,
	EdgeImplements, EdgeImports, EdgeMentions, EdgeModifies, EdgeAuthored,
	EdgeHasFinding, EdgeDependsOn, EdgeAffects, EdgeIncludes, EdgeUsesValue, EdgeReferences,
}

// Valid reports whether k is a known edge kind.
func (k EdgeKind) Valid() bool { return slices.Contains(AllEdgeKinds, k) }

// Node is a vertex in the graph. Fields that do not apply to a kind stay at
// their zero value and are omitted when persisted.
type Node struct {
	ID   string   `json:"id"`
	Kind NodeKind `json:"kind"`
	Repo string   `json:"repo"`
	// Name is the short name (func name, file base name, heading text).
	Name string `json:"name"`
	// QualifiedName is the fully qualified name, e.g. "(*pkg.T).Method".
	QualifiedName string `json:"qualified_name,omitempty"`
	Package       string `json:"package,omitempty"`
	// File is the repository-relative, slash-separated path.
	File      string `json:"file,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Signature string `json:"signature,omitempty"`
	// Summary is a short, redacted description: the first sentence of a Go
	// doc comment, the opening text of a doc section, or a commit subject.
	Summary    string `json:"summary,omitempty"`
	Exported   bool   `json:"exported,omitempty"`
	IsTest     bool   `json:"is_test,omitempty"`
	Complexity int    `json:"complexity,omitempty"`
	LOC        int    `json:"loc,omitempty"`
	// BodyHash fingerprints the declaration source so callers can detect
	// changes without storing the source itself.
	BodyHash string `json:"body_hash,omitempty"`
	// SecurityTags are the sensitive API categories this node touches.
	SecurityTags []string `json:"security_tags,omitempty"`
	// Level is the heading level for Section nodes.
	Level int `json:"level,omitempty"`
	// Time is an RFC 3339 timestamp (commits).
	Time string `json:"time,omitempty"`
	// Churn is the number of indexed commits that touched a File.
	Churn int `json:"churn,omitempty"`
	// Head is the indexed commit for Repository nodes.
	Head string `json:"head,omitempty"`
	// Root is the indexed path for Repository nodes.
	Root string `json:"root,omitempty"`
	// Branch is the indexed branch for Repository nodes ("" when unknown).
	Branch string `json:"branch,omitempty"`
	// Bytes is the size of File and Document nodes.
	Bytes int `json:"bytes,omitempty"`
	// Metrics is the JSON-encoded metrics.Report on Repository nodes.
	Metrics string `json:"-"`

	// Security fields (Finding, Vulnerability, Module nodes).

	// Severity is info, low, medium, high or critical.
	Severity string `json:"severity,omitempty"`
	// Rule is the finding rule ID.
	Rule string `json:"rule,omitempty"`
	// CWE lists weakness IDs such as "CWE-89".
	CWE []string `json:"cwe,omitempty"`
	// Aliases lists other IDs of a vulnerability (CVE, GHSA).
	Aliases []string `json:"aliases,omitempty"`
	// Version is a Module's required version.
	Version string `json:"version,omitempty"`
	// FixedVersion is the first version fixing a Vulnerability.
	FixedVersion string `json:"fixed_version,omitempty"`
	// Reachability is how a Vulnerability is reached: called, imported or required.
	Reachability string `json:"reachability,omitempty"`
	// URL links to the advisory.
	URL string `json:"url,omitempty"`
	// Indirect marks Modules required only indirectly.
	Indirect bool `json:"indirect,omitempty"`
	// Liveness is live, test-only or unreachable for functions and methods,
	// and used or unused for Modules (see package cleanup).
	Liveness string `json:"liveness,omitempty"`
	// Cleanup is the JSON-encoded cleanup.Report on Repository nodes.
	Cleanup string `json:"-"`
	// Language is the source language (go, python, typescript, ...) of code nodes.
	Language string `json:"language,omitempty"`
	// Ecosystem is the package ecosystem of Module nodes (Go, npm, PyPI).
	Ecosystem string `json:"ecosystem,omitempty"`
}

// Severities from lowest to highest.
var Severities = []string{"info", "low", "medium", "high", "critical"}

// SeverityRank orders severities; unknown values rank as -1.
func SeverityRank(s string) int { return slices.Index(Severities, s) }

// Edge is a directed, typed relationship between two node IDs.
type Edge struct {
	From string   `json:"from"`
	To   string   `json:"to"`
	Kind EdgeKind `json:"kind"`
	// Count is how often the relationship occurs (call sites, line changes).
	Count int `json:"count,omitempty"`
}

// NodeID builds a globally unique node ID scoped to a repository. Scoping by
// repo keeps forks and multiple checkouts apart in a shared database.
func NodeID(repo string, kind NodeKind, local string) string {
	return fmt.Sprintf("%s|%s|%s", repo, kind, local)
}

// Graph is an in-memory set of nodes and edges, deduplicated by identity.
// The zero value is not usable; call NewGraph.
type Graph struct {
	nodes     map[string]*Node
	nodeOrder []string
	edges     map[edgeKey]*Edge
	edgeOrder []edgeKey
}

type edgeKey struct {
	from, to string
	kind     EdgeKind
}

// NewGraph returns an empty graph.
func NewGraph() *Graph {
	return &Graph{nodes: map[string]*Node{}, edges: map[edgeKey]*Edge{}}
}

// AddNode inserts n unless a node with the same ID exists. It returns the
// stored node so callers can enrich it.
func (g *Graph) AddNode(n Node) *Node {
	if existing, ok := g.nodes[n.ID]; ok {
		return existing
	}
	stored := n
	g.nodes[n.ID] = &stored
	g.nodeOrder = append(g.nodeOrder, n.ID)
	return &stored
}

// Node returns the node with the given ID.
func (g *Graph) Node(id string) (*Node, bool) {
	n, ok := g.nodes[id]
	return n, ok
}

// AddEdge inserts e, or increments the count of an identical edge.
func (g *Graph) AddEdge(e Edge) {
	if e.Count == 0 {
		e.Count = 1
	}
	k := edgeKey{e.From, e.To, e.Kind}
	if existing, ok := g.edges[k]; ok {
		existing.Count += e.Count
		return
	}
	stored := e
	g.edges[k] = &stored
	g.edgeOrder = append(g.edgeOrder, k)
}

// Nodes returns all nodes in insertion order.
func (g *Graph) Nodes() []Node {
	out := make([]Node, 0, len(g.nodeOrder))
	for _, id := range g.nodeOrder {
		out = append(out, *g.nodes[id])
	}
	return out
}

// Edges returns all edges in insertion order.
func (g *Graph) Edges() []Edge {
	out := make([]Edge, 0, len(g.edgeOrder))
	for _, k := range g.edgeOrder {
		out = append(out, *g.edges[k])
	}
	return out
}

// Merge copies all nodes and edges from other into g.
func (g *Graph) Merge(other *Graph) {
	for _, n := range other.Nodes() {
		g.AddNode(n)
	}
	for _, e := range other.Edges() {
		g.AddEdge(e)
	}
}

// PruneDanglingEdges removes edges whose endpoints are missing. Extractors
// may emit edges to symbols that were filtered out (for example vendored
// code); persisting them would fail or create phantom nodes.
func (g *Graph) PruneDanglingEdges() int {
	kept := g.edgeOrder[:0]
	removed := 0
	for _, k := range g.edgeOrder {
		_, okFrom := g.nodes[k.from]
		_, okTo := g.nodes[k.to]
		if okFrom && okTo {
			kept = append(kept, k)
			continue
		}
		delete(g.edges, k)
		removed++
	}
	g.edgeOrder = kept
	return removed
}

// Location formats the node's position as "file:start-end".
func (n Node) Location() string {
	switch {
	case n.File == "":
		return ""
	case n.StartLine == 0:
		return n.File
	case n.EndLine > n.StartLine:
		return fmt.Sprintf("%s:%d-%d", n.File, n.StartLine, n.EndLine)
	default:
		return fmt.Sprintf("%s:%d", n.File, n.StartLine)
	}
}

// DisplayName returns the most specific human-readable name.
func (n Node) DisplayName() string {
	if n.QualifiedName != "" {
		return n.QualifiedName
	}
	return n.Name
}

var importPathPrefix = regexp.MustCompile(`(?:[A-Za-z0-9_.~-]+/)+`)

// ShortName drops import-path prefixes from a qualified name, turning
// "(*github.com/org/repo/internal/store.Store).Get" into "(*store.Store).Get".
// Answers use it to avoid repeating the module path on every line.
func (n Node) ShortName() string {
	switch n.Kind {
	case KindFile, KindDocument, KindSection, KindPackage, KindCommit, KindAuthor, KindRepository:
		return n.DisplayName()
	}
	return importPathPrefix.ReplaceAllString(n.DisplayName(), "")
}

// Hit is a node reached by a traversal, with its hop distance from the start.
type Hit struct {
	Node  Node `json:"node"`
	Depth int  `json:"depth"`
}

// Stats summarises the contents of one repository's graph.
type Stats struct {
	Repo  string           `json:"repo"`
	Nodes map[NodeKind]int `json:"nodes"`
	Edges map[EdgeKind]int `json:"edges"`
	// Exported and Documented count exported functions, methods and types,
	// and how many of them carry a doc summary.
	Exported   int `json:"exported"`
	Documented int `json:"documented"`
	// Tests counts test functions.
	Tests int `json:"tests"`
	// SecurityTags counts functions per security tag.
	SecurityTags map[string]int `json:"security_tags"`
	// TotalLOC sums lines across File nodes; AvgComplexity is over callables.
	TotalLOC      int     `json:"total_loc"`
	AvgComplexity float64 `json:"avg_complexity"`
	// Formats counts indexed files per language or format: go, python,
	// typescript, javascript, vue, yaml, helm, go-template, markdown.
	Formats map[string]int `json:"formats"`
	// K8sResources counts Kubernetes resources in YAML; HelmCharts charts.
	K8sResources int `json:"k8s_resources"`
	HelmCharts   int `json:"helm_charts"`
}

// FileFormat names the format of a file-level node, or "" for other kinds.
// chart marks templates that belong to a Helm chart.
func FileFormat(kind NodeKind, language string, chart bool) string {
	switch kind {
	case KindFile:
		if language == "" {
			return "go"
		}
		return language
	case KindConfig:
		return "yaml"
	case KindTemplate:
		if chart {
			return "helm"
		}
		return "go-template"
	case KindDocument:
		return "markdown"
	}
	return ""
}

// IsK8sResource reports ConfigEntry nodes that are Kubernetes resources
// (their Signature holds the apiVersion) rather than plain key paths.
func IsK8sResource(n Node) bool {
	if n.Kind != KindConfigEntry {
		return false
	}
	switch n.Signature {
	case "", "map", "list", "scalar", "alias":
		return false
	}
	return true
}

// ContainsFold reports whether s contains substr, ignoring case.
func ContainsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
