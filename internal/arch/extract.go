package arch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/redact"
)

const (
	maxRecordSize = 1 << 20
	maxRecords    = 2000
	summaryLen    = 240
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".nuxt": true, ".output": true, "dist": true, "testdata": true, ".idea": true, ".vscode": true,
}

// dirTypes maps a directory name to the record type of the files in it.
var dirTypes = map[string]string{
	"adr": ADR, "adrs": ADR, "decisions": ADR, "decision-records": ADR,
	"rfc": RFC, "rfcs": RFC, "proposals": RFC,
	"standards": Standard, "policies": Policy, "policy": Policy,
	"runbooks": Runbook, "runbook": Runbook, "playbooks": Runbook,
	"architecture": Overview, "arch": Overview,
}

var diagramExt = map[string]bool{".mmd": true, ".puml": true, ".plantuml": true, ".drawio": true, ".d2": true}

var (
	numberRe  = regexp.MustCompile(`^(\d{1,6})[-_. ]`)
	idRe      = regexp.MustCompile(`(?i)\b(adr|rfc)[\s_-]*0*(\d+)\b`)
	slugBadRe = regexp.MustCompile(`[^A-Za-z0-9._:-]+`)
)

type record struct {
	node    model.Node
	meta    Meta
	rel     string
	fields  map[string][]string
	typ     string
	number  string
	rawRefs map[string][]string
}

// Extract finds the architecture records under root and adds them to g, with
// the Component nodes they name and the edges between records and to the code
// already in g. Run it after the code graph is merged: references to code
// resolve against g. Nothing in a record's text is stored beyond a short
// redacted summary.
func Extract(ctx context.Context, repo, root string, g *model.Graph) error {
	var recs []*record
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") || isRepo(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || len(recs) >= maxRecords {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		typ, candidate := typeOfPath(rel)
		if !candidate {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxRecordSize {
			return nil //nolint:nilerr // unreadable or oversized files are skipped, not fatal
		}
		content, err := os.ReadFile(p) //nolint:gosec // inside root and not a symlink
		if err != nil {
			return fmt.Errorf("reading %s: %w", rel, err)
		}
		if r := readRecord(repo, rel, typ, content); r != nil {
			recs = append(recs, r)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking architecture records: %w", err)
	}
	build(repo, recs, g)
	return nil
}

func isRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// typeOfPath says which type a file would be by its place, and whether it is a
// candidate at all. A front matter "type:" can still decide for files outside
// the usual directories (those are candidates only when they are Markdown).
func typeOfPath(rel string) (typ string, candidate bool) {
	name := path.Base(rel)
	ext := strings.ToLower(path.Ext(name))
	lower := strings.ToLower(rel)
	md := ext == ".md" || ext == ".markdown" || ext == ".mdx"
	segs := strings.Split(strings.ToLower(path.Dir(rel)), "/")
	if diagramExt[ext] {
		for _, s := range segs {
			if s == "architecture" || s == "diagrams" || s == "arch" || s == "adr" || s == "adrs" {
				return Diagram, true
			}
		}
		return "", false
	}
	if !md {
		return "", false
	}
	base := strings.ToLower(strings.TrimSuffix(name, ext))
	if base == "readme" || base == "index" || base == "_index" || strings.Contains(base, "template") {
		return "", false // indexes and templates are not records
	}
	// The innermost directory with a meaning decides.
	for i := len(segs) - 1; i >= 0; i-- {
		if t, ok := dirTypes[segs[i]]; ok {
			return t, true
		}
	}
	if strings.HasPrefix(lower, "architecture.md") || lower == "docs/architecture.md" {
		return Overview, true
	}
	return "", true // Markdown elsewhere: a record only if its front matter says so
}

func readRecord(repo, rel, dirType string, content []byte) *record {
	p := parseDoc(content)
	typ := dirType
	if v := p.Fields["type"]; len(v) > 0 {
		if t := strings.ToLower(strings.TrimSpace(v[0])); slices.Contains(Types, t) {
			typ = t
		}
	}
	if typ == "" {
		return nil
	}
	name := path.Base(rel)
	r := &record{rel: rel, typ: typ, fields: p.Fields}
	if m := numberRe.FindStringSubmatch(name); m != nil {
		r.number = m[1]
	}
	id := ""
	if v := p.Fields["id"]; len(v) > 0 {
		id = v[0]
	}
	if id == "" {
		id = DefaultID(typ, rel, r.number)
	}
	id = slug(id)
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = strings.TrimSuffix(name, path.Ext(name))
	}
	status, rawStatus := "", ""
	if v := p.Fields["status"]; len(v) > 0 {
		rawStatus = v[0]
	}
	switch {
	case rawStatus != "":
		status, _ = Canonical(typ, rawStatus)
	case typ == Overview || typ == Runbook || typ == Diagram:
		status = "current" // informational documents are current until someone says otherwise
	default:
		status = Unknown
	}
	sum := sha256.Sum256(content)
	r.meta = Meta{ID: id, RawStatus: redact.Free(rawStatus), Format: p.Format}
	first := func(k string) string {
		if v := p.Fields[k]; len(v) > 0 {
			return redact.Free(v[0])
		}
		return ""
	}
	r.meta.Owner, r.meta.Date, r.meta.Topic = withoutEmails(first("owner")), first("date"), strings.ToLower(first("topic"))
	for _, l := range p.Fields["labels"] {
		r.meta.Labels = append(r.meta.Labels, strings.ToLower(redact.Free(l)))
	}
	for _, c := range p.Fields["components"] {
		r.meta.Components = append(r.meta.Components, strings.ToLower(redact.Free(c)))
	}
	r.meta.Labels, r.meta.Components = sortedUnique(r.meta.Labels), sortedUnique(r.meta.Components)
	r.rawRefs = map[string][]string{
		"supersedes": p.Fields["supersedes"], "superseded-by": p.Fields["superseded-by"],
		"relates": p.Fields["relates"], "conflicts": p.Fields["conflicts"],
	}
	r.node = model.Node{
		ID: model.NodeID(repo, model.KindRecord, id), Kind: model.KindRecord, Repo: repo, Name: redact.Free(title), QualifiedName: id,
		File: rel, StartLine: 1, EndLine: p.Lines, DocType: typ, Status: status, BodyHash: hex.EncodeToString(sum[:8]),
		Summary: redact.Summary(p.Summary, summaryLen),
	}
	if status == Unknown && rawStatus == "" {
		r.meta.Unresolved = append(r.meta.Unresolved, "no status found: add `status:` front matter or a `- Status:` line")
	} else if status == Unknown {
		r.meta.Unresolved = append(r.meta.Unresolved, fmt.Sprintf("status %q is not a %s status (%s)", rawStatus, typ, strings.Join(statusesOf[typ], ", ")))
	}
	return r
}

var emailRe = regexp.MustCompile(`<?[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}>?`)

// withoutEmails keeps the name of an owner and drops the address: the catalog
// shows who is responsible, and an address is personal data it has no need for.
func withoutEmails(s string) string {
	return strings.Join(strings.Fields(emailRe.ReplaceAllString(s, " ")), " ")
}

// DefaultID is the ID a record gets when its front matter has none: the number
// of its file name for ADRs and RFCs, else the type and its path.
func DefaultID(typ, rel, number string) string {
	if number != "" && (typ == ADR || typ == RFC) {
		n, _ := strconv.Atoi(number)
		return fmt.Sprintf("%s-%04d", strings.ToUpper(typ), n)
	}
	base := strings.TrimSuffix(rel, path.Ext(rel))
	return typ + ":" + strings.ReplaceAll(base, "/", "-")
}

func slug(id string) string {
	s := slugBadRe.ReplaceAllString(strings.TrimSpace(id), "-")
	return strings.Trim(s, "-.")
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	slices.Sort(in)
	return slices.Compact(in)
}

// build resolves references between records and to code and adds everything to g.
func build(repo string, recs []*record, g *model.Graph) {
	// Unique IDs: a second record with the same ID gets a suffix and a note.
	seen := map[string]int{}
	for _, r := range recs {
		seen[r.meta.ID]++
		if n := seen[r.meta.ID]; n > 1 {
			old := r.meta.ID
			r.meta.ID = fmt.Sprintf("%s-%d", old, n)
			r.node.ID = model.NodeID(repo, model.KindRecord, r.meta.ID)
			r.node.QualifiedName = r.meta.ID
			r.meta.Unresolved = append(r.meta.Unresolved, fmt.Sprintf("the ID %s is used by more than one record; this one is %s", old, r.meta.ID))
		}
	}
	byID := map[string]*record{}
	byPath := map[string]*record{}
	byNumber := map[string][]*record{}
	for _, r := range recs {
		byID[strings.ToLower(r.meta.ID)] = r
		byPath[strings.ToLower(r.rel)] = r
		if r.number != "" {
			n, _ := strconv.Atoi(r.number)
			byNumber[strconv.Itoa(n)] = append(byNumber[strconv.Itoa(n)], r)
		}
	}
	resolve := func(from *record, ref string) *record {
		ref = strings.TrimSpace(ref)
		lower := strings.ToLower(ref)
		if r := byID[slug(lower)]; r != nil {
			return r
		}
		if r := byPath[strings.TrimPrefix(lower, "./")]; r != nil {
			return r
		}
		// A relative link such as ../adr/0002-x.md or 0002-x.md.
		for p, r := range byPath {
			if strings.HasSuffix(p, "/"+strings.TrimLeft(lower, "./")) && strings.Contains(lower, ".md") {
				return r
			}
		}
		if m := idRe.FindStringSubmatch(ref); m != nil {
			n, _ := strconv.Atoi(m[2])
			id := strings.ToLower(fmt.Sprintf("%s-%04d", m[1], n))
			if r := byID[id]; r != nil {
				return r
			}
		}
		if n, err := strconv.Atoi(ref); err == nil {
			var same []*record
			for _, r := range byNumber[strconv.Itoa(n)] {
				if r.typ == from.typ {
					same = append(same, r)
				}
			}
			if len(same) == 1 {
				return same[0]
			}
		}
		return nil
	}
	for _, r := range recs {
		for _, kind := range []string{"supersedes", "superseded-by", "relates", "conflicts"} {
			var ids []string
			for _, ref := range r.rawRefs[kind] {
				// Link targets and link texts both arrive; the first that resolves wins.
				t := resolve(r, ref)
				if t == nil {
					if looksLikeRecordRef(ref) {
						r.meta.Unresolved = append(r.meta.Unresolved, fmt.Sprintf("%s: no record matches %q", kind, ref))
					}
					continue
				}
				if t != r {
					ids = append(ids, t.meta.ID)
				}
			}
			ids = sortedUnique(ids)
			switch kind {
			case "supersedes":
				r.meta.Supersedes = ids
			case "superseded-by":
				r.meta.SupersededBy = ids
			case "relates":
				r.meta.Relates = ids
			case "conflicts":
				r.meta.Conflicts = ids
			}
		}
	}
	// A record that names its successor is superseded by it, and the other way round.
	byMetaID := map[string]*record{}
	for _, r := range recs {
		byMetaID[r.meta.ID] = r
	}
	for _, r := range recs {
		for _, old := range r.meta.Supersedes {
			if o := byMetaID[old]; o != nil && !slices.Contains(o.meta.SupersededBy, r.meta.ID) {
				o.meta.SupersededBy = sortedUnique(append(o.meta.SupersededBy, r.meta.ID))
			}
		}
	}
	for _, r := range recs {
		for _, succ := range r.meta.SupersededBy {
			if s := byMetaID[succ]; s != nil && !slices.Contains(s.meta.Supersedes, r.meta.ID) {
				s.meta.Supersedes = sortedUnique(append(s.meta.Supersedes, r.meta.ID))
			}
		}
	}

	idx := newCodeIndex(g)
	for _, r := range recs {
		for _, ref := range r.fields["implements"] {
			hits, why := idx.resolve(ref)
			r.meta.Implements = append(r.meta.Implements, CodeRef{Ref: ref, Resolved: hits})
			if why != "" {
				r.meta.Unresolved = append(r.meta.Unresolved, why)
			}
		}
	}

	for _, r := range recs {
		meta, _ := json.Marshal(r.meta)
		r.node.Meta = string(meta)
		g.AddNode(r.node)
		g.AddEdge(model.Edge{From: model.NodeID(repo, model.KindRepository, repo), To: r.node.ID, Kind: model.EdgeContains})
		for _, comp := range r.meta.Components {
			cid := model.NodeID(repo, model.KindComponent, slug(comp))
			g.AddNode(model.Node{ID: cid, Kind: model.KindComponent, Repo: repo, Name: comp, QualifiedName: slug(comp)})
			g.AddEdge(model.Edge{From: r.node.ID, To: cid, Kind: model.EdgeAppliesTo})
		}
		for _, id := range r.meta.Supersedes {
			g.AddEdge(model.Edge{From: r.node.ID, To: model.NodeID(repo, model.KindRecord, id), Kind: model.EdgeSupersedes})
		}
		for _, id := range r.meta.Relates {
			g.AddEdge(model.Edge{From: r.node.ID, To: model.NodeID(repo, model.KindRecord, id), Kind: model.EdgeRelatesTo})
		}
		for _, c := range r.meta.Implements {
			for _, code := range c.Resolved {
				g.AddEdge(model.Edge{From: code, To: r.node.ID, Kind: model.EdgeImplementsDecision})
			}
		}
	}
}

// looksLikeRecordRef separates "ADR-0002" or "0002-x.md" from prose such as
// "none" or "n/a", which are not broken references.
func looksLikeRecordRef(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "none", "n/a", "na", "-", "tbd", "no":
		return false
	}
	return idRe.MatchString(s) || strings.HasSuffix(s, ".md") || regexp.MustCompile(`^\d+$`).MatchString(s)
}
