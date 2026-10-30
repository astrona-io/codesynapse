package arch

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/astrona-io/codesynapse/internal/ingest/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// The topology groups what makes up one deployable part of the system across
// languages: the image a Dockerfile target builds, the Go main packages that
// target compiles, the Compose services and Kubernetes workloads that run the
// image, and the environment keys the code reads and the manifests set. Each
// part is a member of a Component node with the evidence that put it there.
// A link the files prove (a compose service names the Dockerfile and target,
// a RUN go build names the package, a workload names the same image) is
// confirmed; a link that rests on a name looking alike is a candidate and is
// never presented as proven. Nothing is merged on a guess: two builds that
// would get the same name become two components.
//
// Only names, file positions and key paths are kept. Image references and
// environment values are YAML values and never reach the graph: images are
// compared in memory, and an environment entry keeps its key name only.

// Member roles.
const (
	RoleImageBuild = "image-build"
	RoleGoMain     = "go-main"
	RoleCompose    = "compose-service"
	RoleWorkload   = "workload"
	RoleHelmValues = "helm-values"
)

// Topology is the Meta of a Component node built from deployment files.
type Topology struct {
	Members []Member `json:"members"`
	Env     []EnvKey `json:"env,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// Member is one part of a component and why it belongs.
type Member struct {
	Role string `json:"role"`
	// Name is what the part is called in its file: a package path, a
	// Dockerfile stage, a compose service, "Kind/name container" or a key path.
	Name string `json:"name"`
	// Node is the graph node of the part, when the index has one.
	Node string `json:"node,omitempty"`
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
	// Evidence says what in the files links the part to the component.
	Evidence string `json:"evidence"`
	// Candidate marks a link that rests on similar names, not on the files.
	Candidate bool `json:"candidate,omitempty"`
}

// EnvKey is an environment variable a component reads or is given, by name.
type EnvKey struct {
	Name   string   `json:"name"`
	ReadBy []EnvUse `json:"read_by,omitempty"`
	SetBy  []EnvUse `json:"set_by,omitempty"`
}

// EnvUse is where a key is read (Via is the package) or set (Via is the member).
type EnvUse struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Via       string `json:"via,omitempty"`
	Candidate bool   `json:"candidate,omitempty"`
}

// ComponentKey is the name a component is known by in the graph: the slug
// its node ID is made from, in lower case. Record front matter and the
// topology both name components through it, so they meet on one node.
func ComponentKey(name string) string {
	return slug(strings.ToLower(strings.TrimSpace(name)))
}

// TopologyOf decodes the topology of a Component node. ok is false for a
// component that only records name.
func TopologyOf(n model.Node) (Topology, bool) {
	if n.Kind != model.KindComponent || n.Meta == "" {
		return Topology{}, false
	}
	var t Topology
	if err := json.Unmarshal([]byte(n.Meta), &t); err != nil || len(t.Members) == 0 {
		return Topology{}, false
	}
	return t, true
}

const (
	maxTopoFile   = 1 << 20
	maxUnits      = 200
	maxReachable  = 500
	maxGoFiles    = 5000
	maxEnvKeys    = 100
	maxEnvUses    = 3
	maxMembers    = 100
	maxContainers = 50
)

var (
	envNameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	shellSepRe  = regexp.MustCompile(`&&|\|\||;|\n`)
	workloadSet = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true, "Job": true, "CronJob": true, "Pod": true, "ReplicaSet": true}
)

// goBuild is a RUN go build of a package directory in a Dockerfile stage.
type goBuild struct {
	dir  string // as written, cleaned: "services/api/cmd/x" or "."
	out  string // -o path, "" when none
	line int
}

type copyFrom struct {
	stage int
	src   []string
}

type dstage struct {
	name   string
	line   int
	base   int // index of the stage FROM names, -1 for an image
	builds []goBuild
	copies []copyFrom
}

type dockerfile struct {
	rel    string
	stages []dstage
	byName map[string]int
}

// unit is one build target: a Dockerfile stage that becomes an image.
type unit struct {
	df       *dockerfile
	stage    int
	context  string // build context the compose file names, "" when unknown
	services []composeService
	images   []string // image repositories (no tag) the building services name
	name     string
}

type composeService struct {
	file, name, node string
	line             int
	dockerfile       string // repo-relative, "" when it builds nothing
	target           string
	context          string
	image            string
	env              []envEntry
}

type envEntry struct {
	name string
	line int
}

type container struct {
	file, workload, name, node string
	line                       int
	image                      string
	env                        []envEntry
}

type helmImage struct {
	file, key, node string
	line            int
	image           string
}

type topoIndex struct {
	root     string
	repo     string
	g        *model.Graph
	pkgByDir map[string]string   // directory -> Go package path (non-test files)
	mains    map[string]bool     // package paths of main packages
	pkgFiles map[string][]string // package path -> its non-test Go files
	imports  map[string][]string // package path -> in-repo imports
	envReads map[string][]envRead
	goFiles  int // Go files read for environment keys, capped at maxGoFiles
}

type envRead struct {
	name, file string
	line       int
}

// BuildTopology adds a Component node for every image build target the
// repository's Dockerfiles define, with the Go main packages, compose
// services, Kubernetes workloads and Helm values that belong to it and the
// environment keys involved. Run it after the code and configuration graphs
// are merged into g: it reads only files g already holds, so the repository
// policy that applied to them applies here too.
func BuildTopology(ctx context.Context, repo, root string, g *model.Graph) error {
	x := &topoIndex{root: root, repo: repo, g: g, pkgByDir: map[string]string{}, mains: map[string]bool{},
		pkgFiles: map[string][]string{}, imports: map[string][]string{}, envReads: map[string][]envRead{}}
	var dockerRels, composeRels, k8sRels, valuesRels []string
	for _, n := range g.Nodes() {
		switch {
		case n.Kind == model.KindPackage && n.Name == "main":
			x.mains[n.QualifiedName] = true
		case n.Kind == model.KindFile && strings.HasSuffix(n.File, ".go") && !n.IsTest && n.Package != "":
			x.pkgByDir[path.Dir(n.File)] = n.Package
			x.pkgFiles[n.Package] = append(x.pkgFiles[n.Package], n.File)
		case n.Kind == model.KindConfig && n.Language == "dockerfile":
			dockerRels = append(dockerRels, n.File)
		case n.Kind == model.KindConfig && isYAML(n.File):
			switch {
			case config.IsCompose(n.File):
				composeRels = append(composeRels, n.File)
			case strings.HasPrefix(n.Summary, "Kubernetes:"):
				k8sRels = append(k8sRels, n.File)
			case n.Summary == "Helm values":
				valuesRels = append(valuesRels, n.File)
			}
		}
	}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeImports {
			from, to := pkgOf(e.From), pkgOf(e.To)
			x.imports[from] = append(x.imports[from], to)
		}
	}
	for _, s := range [][]string{dockerRels, composeRels, k8sRels, valuesRels} {
		slices.Sort(s)
	}
	for p := range x.pkgFiles {
		slices.Sort(x.pkgFiles[p])
	}
	for p := range x.imports {
		x.imports[p] = slices.Compact(slices.Sorted(slices.Values(x.imports[p])))
	}

	dfs := map[string]*dockerfile{}
	for _, rel := range dockerRels {
		if err := ctx.Err(); err != nil {
			return err
		}
		if content, ok := x.read(rel); ok {
			dfs[rel] = parseStages(rel, content)
		}
	}
	if len(dfs) == 0 {
		return nil
	}
	var services []composeService
	for _, rel := range composeRels {
		if content, ok := x.read(rel); ok {
			services = append(services, x.composeServices(rel, content)...)
		}
	}
	var containers []container
	for _, rel := range k8sRels {
		if err := ctx.Err(); err != nil {
			return err
		}
		if content, ok := x.read(rel); ok {
			containers = append(containers, x.workloads(rel, content)...)
		}
	}
	var helm []helmImage
	for _, rel := range valuesRels {
		if content, ok := x.read(rel); ok {
			helm = append(helm, x.helmImages(rel, content)...)
		}
	}

	units := buildUnits(dfs, services)
	nameUnits(units)
	for _, u := range units {
		if err := ctx.Err(); err != nil {
			return err
		}
		x.addComponent(u, units, services, containers, helm)
	}
	return nil
}

func isYAML(rel string) bool {
	ext := strings.ToLower(path.Ext(rel))
	return ext == ".yaml" || ext == ".yml"
}

// pkgOf takes the package path out of a Package node ID (repo|Package|path).
func pkgOf(id string) string {
	parts := strings.SplitN(id, "|", 3)
	if len(parts) < 3 {
		return id
	}
	return parts[2]
}

// read returns a file of the index, refusing anything that is not a plain
// file inside root or is larger than maxTopoFile.
func (x *topoIndex) read(rel string) ([]byte, bool) {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, false
	}
	p := filepath.Join(x.root, filepath.FromSlash(rel))
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxTopoFile {
		return nil, false
	}
	b, err := os.ReadFile(p) //nolint:gosec // a regular file inside root, named by the index
	return b, err == nil
}

// parseStages reads the stages of a Dockerfile, the Go packages each one
// builds and what it copies from earlier stages.
func parseStages(rel string, content []byte) *dockerfile {
	df := &dockerfile{rel: rel, byName: map[string]int{}}
	for _, in := range config.ParseDockerfile(content) {
		switch in.Cmd {
		case "FROM":
			s := dstage{line: in.Line, base: -1}
			fields := noFlags(strings.Fields(in.Args))
			if len(fields) > 0 {
				if j, ok := df.byName[strings.ToLower(fields[0])]; ok {
					s.base = j
				}
			}
			if len(fields) >= 3 && strings.EqualFold(fields[1], "AS") {
				s.name = strings.ToLower(fields[2])
				df.byName[s.name] = len(df.stages)
			}
			df.stages = append(df.stages, s)
		case "RUN":
			if len(df.stages) > 0 {
				s := &df.stages[len(df.stages)-1]
				s.builds = append(s.builds, goBuilds(in.Args, in.Line)...)
			}
		case "COPY":
			if len(df.stages) == 0 {
				continue
			}
			fields := strings.Fields(in.Args)
			from := ""
			for _, f := range fields {
				if v, ok := strings.CutPrefix(f, "--from="); ok {
					from = strings.ToLower(v)
				}
			}
			j, ok := df.byName[from]
			if !ok {
				continue // another image, or a stage by number: not followed
			}
			src := noFlags(fields)
			if len(src) > 1 {
				src = src[:len(src)-1] // the last is the destination
			}
			s := &df.stages[len(df.stages)-1]
			s.copies = append(s.copies, copyFrom{stage: j, src: src})
		}
	}
	return df
}

func noFlags(fields []string) []string {
	out := fields[:0:0]
	for _, f := range fields {
		if !strings.HasPrefix(f, "--") {
			out = append(out, f)
		}
	}
	return out
}

// goBuilds finds "go build" and "go install" commands in a RUN line and the
// local package directories they name. Patterns with "..." and directories
// with variables are left out: they name no single package.
func goBuilds(args string, line int) []goBuild {
	var out []goBuild
	for _, seg := range shellSepRe.Split(args, -1) {
		f := strings.Fields(seg)
		for i := 0; i+1 < len(f); i++ {
			if f[i] != "go" || (f[i+1] != "build" && f[i+1] != "install") {
				continue
			}
			var b goBuild
			var dirs []string
			for k := i + 2; k < len(f); k++ {
				t := f[k]
				switch {
				case t == "-o" && k+1 < len(f):
					b.out = strings.Trim(f[k+1], `"'`)
					k++
				case strings.HasPrefix(t, "-o="):
					b.out = strings.Trim(strings.TrimPrefix(t, "-o="), `"'`)
				case (t == "." || strings.HasPrefix(t, "./")) && !strings.Contains(t, "...") && !strings.ContainsAny(t, "$\"'"):
					dirs = append(dirs, path.Clean(t))
				}
			}
			for _, d := range dirs {
				out = append(out, goBuild{dir: d, out: b.out, line: line})
			}
			break
		}
	}
	return out
}

// mainLink is a Go package a stage's image carries.
type mainLink struct {
	build     goBuild
	stage     int
	candidate bool
	via       string
}

// stageBuilds returns the Go builds an image of stage i carries: built in it,
// in the stage it starts FROM, or copied in from another stage. A copy that
// names the build's -o path (or a directory holding it) is proof, a copy
// that names other paths leaves the build out, and a build without -o is
// only a candidate: nothing says which file the copy takes.
func (df *dockerfile) stageBuilds(i int, seen map[int]bool) []mainLink {
	if seen[i] || i < 0 || i >= len(df.stages) {
		return nil
	}
	seen[i] = true
	s := df.stages[i]
	var out []mainLink
	for _, b := range s.builds {
		out = append(out, mainLink{build: b, stage: i})
	}
	if s.base >= 0 {
		out = append(out, df.stageBuilds(s.base, seen)...)
	}
	for _, c := range s.copies {
		for _, m := range df.stageBuilds(c.stage, seen) {
			if m.build.out != "" && !copied(m.build.out, c.src) {
				continue // the copy names other files
			}
			m.candidate = m.candidate || m.build.out == ""
			m.via = "copied from stage " + df.stages[c.stage].name
			out = append(out, m)
		}
	}
	return out
}

func copied(out string, src []string) bool {
	if out == "" {
		return false
	}
	for _, s := range src {
		if s == out || strings.HasPrefix(out, strings.TrimSuffix(s, "/")+"/") {
			return true
		}
	}
	return false
}

// targets are the stages that look like build targets: the final one and
// every named stage no other stage starts from or copies from.
func (df *dockerfile) targets() []int {
	used := map[int]bool{}
	for _, s := range df.stages {
		if s.base >= 0 {
			used[s.base] = true
		}
		for _, c := range s.copies {
			used[c.stage] = true
		}
	}
	var out []int
	for i, s := range df.stages {
		if i == len(df.stages)-1 || (s.name != "" && !used[i]) {
			out = append(out, i)
		}
	}
	return out
}

// buildUnits makes one unit per build target, attaching the compose
// services that build it.
func buildUnits(dfs map[string]*dockerfile, services []composeService) []*unit {
	byKey := map[string]*unit{}
	var units []*unit
	add := func(df *dockerfile, stage int) *unit {
		key := fmt.Sprintf("%s#%d", df.rel, stage)
		if u := byKey[key]; u != nil {
			return u
		}
		u := &unit{df: df, stage: stage}
		byKey[key] = u
		units = append(units, u)
		return u
	}
	for _, rel := range slices.Sorted(maps.Keys(dfs)) {
		df := dfs[rel]
		for _, i := range df.targets() {
			add(df, i)
		}
	}
	for _, s := range services {
		df := dfs[s.dockerfile]
		if df == nil || len(df.stages) == 0 {
			continue
		}
		stage := len(df.stages) - 1
		if s.target != "" {
			j, ok := df.byName[strings.ToLower(s.target)]
			if !ok {
				continue
			}
			stage = j
		}
		u := add(df, stage)
		u.services = append(u.services, s)
		if u.context == "" {
			u.context = s.context
		}
		if repo, ok := imageRepo(s.image); ok {
			u.images = append(u.images, repo)
		}
	}
	slices.SortFunc(units, func(a, b *unit) int { return cmp.Or(cmp.Compare(a.df.rel, b.df.rel), cmp.Compare(a.stage, b.stage)) })
	if len(units) > maxUnits {
		units = units[:maxUnits]
	}
	for _, u := range units {
		u.images = slices.Compact(slices.Sorted(slices.Values(u.images)))
	}
	return units
}

// nameUnits names each unit: the compose service that builds it (the one
// named like the target first), else the stage name, else the Dockerfile's
// directory. Units that would share a name get a number: they are never
// merged on a name.
func nameUnits(units []*unit) {
	used := map[string]int{}
	for _, u := range units {
		s := u.df.stages[u.stage]
		name := ""
		var svc []string
		for _, c := range u.services {
			svc = append(svc, strings.ToLower(c.name))
		}
		slices.Sort(svc)
		switch {
		case s.name != "" && slices.Contains(svc, s.name):
			name = s.name
		case len(svc) > 0:
			name = svc[0]
		case s.name != "" && s.name != "build" && s.name != "final" && s.name != "runtime":
			name = s.name
		default:
			dir := path.Base(path.Dir(u.df.rel))
			if dir == "." || dir == "/" {
				dir = strings.TrimSuffix(strings.ToLower(path.Base(u.df.rel)), ".dockerfile")
			}
			name = strings.ToLower(dir)
		}
		name = slug(name)
		if name == "" {
			name = "image"
		}
		used[name]++
		if n := used[name]; n > 1 {
			name = fmt.Sprintf("%s-%d", name, n)
		}
		u.name = name
	}
}

// imageRepo is the repository part of an image reference, lower case,
// without tag or digest. ok is false when it holds a variable.
func imageRepo(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.Contains(ref, "{{") || strings.Contains(ref, "$") {
		return "", false
	}
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref = ref[:i]
	}
	return strings.ToLower(ref), ref != ""
}

// imageBase is the last path segment of an image reference; it may still be
// read when the registry part is a variable.
func imageBase(ref string) string {
	ref, _, _ = strings.Cut(strings.TrimSpace(ref), "@")
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		ref = ref[i+1:]
	}
	ref, _, _ = strings.Cut(ref, ":")
	if ref == "" || strings.ContainsAny(ref, "${}") {
		return ""
	}
	return strings.ToLower(ref)
}

func decodeYAML(content []byte) []*yaml.Node {
	dec := yaml.NewDecoder(bytes.NewReader(content))
	var docs []*yaml.Node
	for len(docs) < 1000 {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if !errors.Is(err, io.EOF) {
				return docs
			}
			break
		}
		if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 && doc.Content[0].Kind == yaml.MappingNode {
			docs = append(docs, doc.Content[0])
		}
	}
	return docs
}

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil {
		return nil
	}
	if m.Kind == yaml.AliasNode {
		m = m.Alias
	}
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if v.Kind == yaml.AliasNode && v.Alias != nil {
				return v.Alias
			}
			return v
		}
	}
	return nil
}

func str(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// composeServices reads the services of a compose file: what they build,
// the image they name and the keys of their environment.
func (x *topoIndex) composeServices(rel string, content []byte) []composeService {
	var out []composeService
	dir := path.Dir(rel)
	for _, doc := range decodeYAML(content) {
		svcs := get(doc, "services")
		if svcs == nil || svcs.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(svcs.Content); i += 2 {
			k, v := svcs.Content[i], svcs.Content[i+1]
			s := composeService{file: rel, name: k.Value, line: k.Line, image: str(get(v, "image"))}
			if id := model.NodeID(x.repo, model.KindConfigEntry, rel+"#services."+k.Value); x.has(id) {
				s.node = id
			}
			if b := get(v, "build"); b != nil {
				ctxDir, file := b.Value, "Dockerfile"
				if b.Kind != yaml.ScalarNode {
					ctxDir = cmp.Or(str(get(b, "context")), ".")
					file = cmp.Or(str(get(b, "dockerfile")), file)
					s.target = str(get(b, "target"))
				}
				if !strings.Contains(ctxDir, "://") && !strings.ContainsAny(ctxDir+file, "${}") {
					s.context = path.Clean(path.Join(dir, ctxDir))
					s.dockerfile = path.Clean(path.Join(s.context, file))
				}
			}
			s.env = composeEnv(get(v, "environment"))
			out = append(out, s)
		}
	}
	return out
}

// composeEnv reads the key names of a compose environment, as a map (with
// merge keys) or as a list of KEY=value items. Values are never read.
func composeEnv(n *yaml.Node) []envEntry {
	return composeEnvDepth(n, 0)
}

func composeEnvDepth(n *yaml.Node, depth int) []envEntry {
	if n == nil || depth > 8 {
		return nil
	}
	var out []envEntry
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "<<" {
				if v.Kind == yaml.AliasNode {
					v = v.Alias
				}
				out = append(out, composeEnvDepth(v, depth+1)...)
				continue
			}
			if envNameRe.MatchString(k.Value) {
				out = append(out, envEntry{name: k.Value, line: k.Line})
			}
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			name, _, _ := strings.Cut(str(item), "=")
			if envNameRe.MatchString(name) {
				out = append(out, envEntry{name: name, line: item.Line})
			}
		}
	}
	return out
}

// workloads reads the containers of the Kubernetes workloads in a manifest:
// their image and the names of their env entries.
func (x *topoIndex) workloads(rel string, content []byte) []container {
	var out []container
	for _, doc := range decodeYAML(content) {
		kind := str(get(doc, "kind"))
		if !workloadSet[kind] || str(get(doc, "apiVersion")) == "" {
			continue
		}
		name := cmp.Or(str(get(get(doc, "metadata"), "name")), "(unnamed)")
		res := kind + "/" + name
		id := model.NodeID(x.repo, model.KindConfigEntry, rel+"#"+res)
		if !x.has(id) {
			continue // the policy kept it out of the index
		}
		spec := get(doc, "spec")
		switch kind {
		case "CronJob":
			spec = get(get(get(get(spec, "jobTemplate"), "spec"), "template"), "spec")
		case "Pod":
		default:
			spec = get(get(spec, "template"), "spec")
		}
		for _, list := range []string{"initContainers", "containers"} {
			cs := get(spec, list)
			if cs == nil || cs.Kind != yaml.SequenceNode {
				continue
			}
			for _, c := range cs.Content {
				if len(out) >= maxContainers*10 {
					return out
				}
				ct := container{file: rel, workload: res, name: str(get(c, "name")), node: id, line: c.Line, image: str(get(c, "image"))}
				if env := get(c, "env"); env != nil && env.Kind == yaml.SequenceNode {
					for _, e := range env.Content {
						if n := get(e, "name"); n != nil && envNameRe.MatchString(n.Value) {
							ct.env = append(ct.env, envEntry{name: n.Value, line: n.Line})
						}
					}
				}
				out = append(out, ct)
			}
		}
	}
	return out
}

// helmImages finds image settings in a chart's values: an "image" key that
// is a reference or a map with "repository".
func (x *topoIndex) helmImages(rel string, content []byte) []helmImage {
	var out []helmImage
	var walk func(m *yaml.Node, prefix string, depth int)
	walk = func(m *yaml.Node, prefix string, depth int) {
		if m == nil || m.Kind != yaml.MappingNode || depth > 6 || len(out) >= maxContainers {
			return
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			k, v := m.Content[i], m.Content[i+1]
			p := k.Value
			if prefix != "" {
				p = prefix + "." + k.Value
			}
			if k.Value == "image" {
				img := str(v)
				if img == "" {
					img = str(get(v, "repository"))
				}
				if img != "" {
					h := helmImage{file: rel, key: p, line: k.Line, image: img}
					if id := model.NodeID(x.repo, model.KindConfigEntry, rel+"#"+p); x.has(id) {
						h.node = id
					} else {
						h.node = model.NodeID(x.repo, model.KindConfig, rel)
					}
					out = append(out, h)
				}
				continue
			}
			walk(v, p, depth+1)
		}
	}
	for _, doc := range decodeYAML(content) {
		walk(doc, "", 0)
	}
	return out
}

func (x *topoIndex) has(id string) bool {
	_, ok := x.g.Node(id)
	return ok
}

// imageMatch says whether ref is the image a unit builds: exactly the
// repository a building service names (proof), or only the same last segment
// as that repository or the unit's name (a candidate).
func imageMatch(u *unit, ref string) (match, candidate bool) {
	if repo, ok := imageRepo(ref); ok && slices.Contains(u.images, repo) {
		return true, false
	}
	base := imageBase(ref)
	if base == "" {
		return false, false
	}
	if base == u.name {
		return true, true
	}
	for _, img := range u.images {
		if imageBase(img) == base {
			return true, true
		}
	}
	return false, false
}

func (x *topoIndex) addComponent(u *unit, units []*unit, services []composeService, containers []container, helm []helmImage) {
	var t Topology
	df, st := u.df, u.df.stages[u.stage]
	stageName := cmp.Or(st.name, "final stage")
	add := func(m Member) {
		if len(t.Members) >= maxMembers {
			return
		}
		t.Members = append(t.Members, m)
	}
	add(Member{Role: RoleImageBuild, Name: stageName, Node: model.NodeID(x.repo, model.KindConfig, df.rel), File: df.rel, Line: st.line,
		Evidence: fmt.Sprintf("build target %s of %s", stageName, df.rel)})

	// Go main packages the image carries.
	type mainHit struct {
		pkg       string
		candidate bool
		member    int // index in t.Members, -1 when the cap left it out
	}
	var mains []mainHit
	seenPkg := map[string]int{}
	for _, m := range df.stageBuilds(u.stage, map[int]bool{}) {
		pkg, guessed := x.resolveBuild(u, m.build.dir)
		if pkg == "" || !x.mains[pkg] {
			continue
		}
		cand := m.candidate || guessed
		ev := fmt.Sprintf("go build ./%s at %s:%d", m.build.dir, df.rel, m.build.line)
		if m.via != "" {
			ev += ", " + m.via
		}
		if guessed {
			ev += "; built from the Dockerfile's directory, the build context is not known"
		} else if m.candidate {
			ev += "; the copy does not name the built binary"
		}
		if i, ok := seenPkg[pkg]; ok {
			if h := &mains[i]; h.candidate && !cand {
				h.candidate = false
				if h.member >= 0 {
					t.Members[h.member].Candidate, t.Members[h.member].Evidence = false, ev
				}
			}
			continue
		}
		seenPkg[pkg] = len(mains)
		member := -1
		if len(t.Members) < maxMembers {
			member = len(t.Members)
		}
		mains = append(mains, mainHit{pkg: pkg, candidate: cand, member: member})
		add(Member{Role: RoleGoMain, Name: pkg, Node: model.NodeID(x.repo, model.KindPackage, pkg), File: firstOf(x.pkgFiles[pkg]),
			Evidence: ev, Candidate: cand})
	}

	type setter struct {
		via       string
		file      string
		env       []envEntry
		candidate bool
	}
	var setters []setter
	for _, s := range u.services {
		add(Member{Role: RoleCompose, Name: s.name, Node: s.node, File: s.file, Line: s.line,
			Evidence: fmt.Sprintf("builds %s target %s", df.rel, stageName)})
		setters = append(setters, setter{via: s.name, file: s.file, env: s.env})
	}
	// Services that only run an image: the image the unit's services name.
	for _, s := range services {
		if s.dockerfile != "" || s.image == "" {
			continue
		}
		if ok, cand := imageMatch(u, s.image); ok && uniqueMatch(u, units, s.image, cand) {
			add(Member{Role: RoleCompose, Name: s.name, Node: s.node, File: s.file, Line: s.line, Candidate: cand, Evidence: imageEvidence(cand)})
			setters = append(setters, setter{via: s.name, file: s.file, env: s.env, candidate: cand})
		}
	}
	for _, c := range containers {
		if ok, cand := imageMatch(u, c.image); ok && uniqueMatch(u, units, c.image, cand) {
			name := c.workload
			if c.name != "" {
				name += " container " + c.name
			}
			add(Member{Role: RoleWorkload, Name: name, Node: c.node, File: c.file, Line: c.line, Candidate: cand, Evidence: imageEvidence(cand)})
			setters = append(setters, setter{via: name, file: c.file, env: c.env, candidate: cand})
		}
	}
	for _, h := range helm {
		if ok, _ := imageMatch(u, h.image); ok && uniqueMatch(u, units, h.image, true) {
			add(Member{Role: RoleHelmValues, Name: h.key, Node: h.node, File: h.file, Line: h.line, Candidate: true,
				Evidence: "the chart's values name an image like this component's; its templates decide which workload runs it"})
		}
	}

	// Environment keys: read by the Go code the image carries, set by the
	// services and workloads that run it.
	keys := map[string]*EnvKey{}
	key := func(name string) *EnvKey {
		k := keys[name]
		if k == nil {
			k = &EnvKey{Name: name}
			keys[name] = k
		}
		return k
	}
	for _, m := range mains {
		for _, pkg := range x.reachable(m.pkg) {
			for _, r := range x.readsOf(pkg) {
				k := key(r.name)
				if len(k.ReadBy) < maxEnvUses {
					k.ReadBy = append(k.ReadBy, EnvUse{File: r.file, Line: r.line, Via: pkg, Candidate: m.candidate})
				}
			}
		}
	}
	for _, s := range setters {
		for _, e := range s.env {
			k := key(e.name)
			if len(k.SetBy) < maxEnvUses {
				k.SetBy = append(k.SetBy, EnvUse{File: s.file, Line: e.line, Via: s.via, Candidate: s.candidate})
			}
		}
	}
	names := slices.Sorted(maps.Keys(keys))
	if len(names) > maxEnvKeys {
		t.Notes = append(t.Notes, fmt.Sprintf("%d environment keys, the first %d by name are listed", len(names), maxEnvKeys))
		names = names[:maxEnvKeys]
	}
	for _, n := range names {
		k := keys[n]
		sortUses(k.ReadBy)
		sortUses(k.SetBy)
		t.Env = append(t.Env, *k)
	}
	if len(t.Members) >= maxMembers {
		t.Notes = append(t.Notes, fmt.Sprintf("more than %d members; the rest are left out", maxMembers))
	}

	meta, err := json.Marshal(t)
	if err != nil {
		return
	}
	id := model.NodeID(x.repo, model.KindComponent, u.name)
	n := x.g.AddNode(model.Node{ID: id, Kind: model.KindComponent, Repo: x.repo, Name: u.name, QualifiedName: u.name})
	n.Meta = string(meta)
	n.File, n.StartLine = df.rel, st.line
	n.Summary = redact.Summary(summaryOf(t), summaryLen)
	for _, m := range t.Members {
		if m.Node == "" || !x.has(m.Node) {
			continue
		}
		ev := model.EvidenceSyntactic
		if m.Candidate {
			ev = model.EvidenceInferred
		}
		x.g.AddEdge(model.Edge{From: id, To: m.Node, Kind: model.EdgeAppliesTo, Evidence: ev})
	}
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func sortUses(u []EnvUse) {
	slices.SortFunc(u, func(a, b EnvUse) int { return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line)) })
}

func imageEvidence(candidate bool) string {
	if candidate {
		return "image name ends like this component's; not proven to be the image it builds"
	}
	return "runs the image a compose service names for this build"
}

// uniqueMatch keeps a proven image match, and a candidate only when no other
// unit matches the image at all: two builds that look alike are left apart.
func uniqueMatch(u *unit, units []*unit, ref string, candidate bool) bool {
	if !candidate {
		return true
	}
	for _, o := range units {
		if o == u {
			continue
		}
		if ok, _ := imageMatch(o, ref); ok {
			return false
		}
	}
	return true
}

func summaryOf(t Topology) string {
	counts := map[string]int{}
	cand := 0
	for _, m := range t.Members {
		counts[m.Role]++
		if m.Candidate {
			cand++
		}
	}
	var parts []string
	for _, r := range []string{RoleGoMain, RoleCompose, RoleWorkload, RoleHelmValues} {
		if counts[r] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[r], r))
		}
	}
	s := "Image build"
	if len(parts) > 0 {
		s += " with " + strings.Join(parts, ", ")
	}
	if len(t.Env) > 0 {
		s += fmt.Sprintf("; %d environment keys", len(t.Env))
	}
	if cand > 0 {
		s += fmt.Sprintf("; %d candidate links", cand)
	}
	return s
}

// resolveBuild maps a go build directory to a package: against the build
// context the compose file names, else the repository root. Only when both
// fail is the Dockerfile's own directory tried, and that is a guess.
func (x *topoIndex) resolveBuild(u *unit, dir string) (pkg string, guessed bool) {
	for _, base := range []string{u.context, "."} {
		if base == "" {
			continue
		}
		if p, ok := x.pkgByDir[path.Clean(path.Join(base, dir))]; ok {
			return p, false
		}
	}
	if p, ok := x.pkgByDir[path.Clean(path.Join(path.Dir(u.df.rel), dir))]; ok {
		return p, true
	}
	return "", false
}

// reachable is the main package and the in-repo packages it imports,
// breadth first, sorted.
func (x *topoIndex) reachable(main string) []string {
	seen := map[string]bool{main: true}
	queue := []string{main}
	for len(queue) > 0 && len(seen) < maxReachable {
		p := queue[0]
		queue = queue[1:]
		for _, imp := range x.imports[p] {
			if !seen[imp] && len(seen) < maxReachable {
				seen[imp] = true
				queue = append(queue, imp)
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// readsOf returns the environment keys a package reads with os.Getenv or
// os.LookupEnv, given as a string literal or a string constant of the package.
func (x *topoIndex) readsOf(pkg string) []envRead {
	if r, ok := x.envReads[pkg]; ok {
		return r
	}
	fset := token.NewFileSet()
	var files []*ast.File
	var rels []string
	for _, rel := range x.pkgFiles[pkg] {
		if x.goFiles >= maxGoFiles {
			break
		}
		x.goFiles++
		content, ok := x.read(rel)
		if !ok || (!bytes.Contains(content, []byte("Getenv")) && !bytes.Contains(content, []byte("LookupEnv"))) {
			continue
		}
		f, err := parser.ParseFile(fset, rel, content, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		files, rels = append(files, f), append(rels, rel)
	}
	consts := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if s, ok := stringLit(vs.Values[i]); ok {
							consts[name.Name] = s
						}
					}
				}
			}
		}
	}
	var out []envRead
	for i, f := range files {
		osName := ""
		for _, imp := range f.Imports {
			if imp.Path.Value == `"os"` {
				osName = "os"
				if imp.Name != nil {
					osName = imp.Name.Name
				}
			}
		}
		if osName == "" || osName == "_" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != osName {
				return true
			}
			name, ok := stringLit(call.Args[0])
			if !ok {
				if id, isID := call.Args[0].(*ast.Ident); isID {
					name, ok = consts[id.Name]
				}
			}
			if ok && envNameRe.MatchString(name) {
				out = append(out, envRead{name: name, file: rels[i], line: fset.Position(call.Pos()).Line})
			}
			return true
		})
	}
	slices.SortFunc(out, func(a, b envRead) int {
		return cmp.Or(cmp.Compare(a.name, b.name), cmp.Compare(a.file, b.file), cmp.Compare(a.line, b.line))
	})
	x.envReads[pkg] = out
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
