package review

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// New versus fixed findings (docs/PLAN.md, security roadmap item 1 and
// agent workflow step 6). A change is judged by what it adds and removes,
// not by the backlog in the files it touches: the findings of the changed
// files are read from a graph of the base and one of the head and
// compared by fingerprint (rule, owning symbol and file, no line numbers,
// which every edit above shifts). Equal fingerprints are counted, so a
// third copy of the same problem in one function is still new.

// DeltaFinding is one finding a change adds or removes.
type DeltaFinding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Owner    string `json:"owner"`
	Location string `json:"location"`
}

// SecurityDelta is what a change does to the security findings of the
// files it touches.
type SecurityDelta struct {
	// Introduced and Fixed count findings by severity.
	Introduced map[string]int `json:"introduced"`
	Fixed      map[string]int `json:"fixed"`
	// Unchanged counts findings present before and after.
	Unchanged int            `json:"unchanged"`
	New       []DeltaFinding `json:"new"`
	Removed   []DeltaFinding `json:"removed"`
	// Base names what the change was compared with, e.g. a commit.
	Base string `json:"base,omitempty"`
}

// maxDeltaFindings bounds the findings read per file and side.
const maxDeltaFindings = 200

// CompareFindings reads the findings of files from the base and head graphs
// of repo and returns what the change introduced and fixed.
func CompareFindings(ctx context.Context, base, head store.Reader, repo string, files []string) (SecurityDelta, error) {
	before, err := findingsIn(ctx, base, repo, files)
	if err != nil {
		return SecurityDelta{}, err
	}
	after, err := findingsIn(ctx, head, repo, files)
	if err != nil {
		return SecurityDelta{}, err
	}
	d := SecurityDelta{Introduced: map[string]int{}, Fixed: map[string]int{}, New: []DeltaFinding{}, Removed: []DeltaFinding{}}
	left := map[string][]DeltaFinding{}
	for _, f := range before {
		left[fingerprint(f)] = append(left[fingerprint(f)], f)
	}
	for _, f := range after {
		key := fingerprint(f)
		if len(left[key]) > 0 {
			left[key] = left[key][1:]
			d.Unchanged++
			continue
		}
		d.New = append(d.New, f)
		d.Introduced[f.Severity]++
	}
	for _, fs := range left {
		for _, f := range fs {
			d.Removed = append(d.Removed, f)
			d.Fixed[f.Severity]++
		}
	}
	for _, list := range [][]DeltaFinding{d.New, d.Removed} {
		slices.SortFunc(list, func(a, b DeltaFinding) int {
			return cmp.Or(cmp.Compare(model.SeverityRank(b.Severity), model.SeverityRank(a.Severity)), cmp.Compare(a.Location, b.Location), cmp.Compare(a.Rule, b.Rule))
		})
	}
	return d, nil
}

func fingerprint(f DeltaFinding) string {
	file, _, _ := strings.Cut(f.Location, ":")
	return f.Rule + "\x00" + f.Owner + "\x00" + file
}

// Findings lists the security findings in files with the symbol that owns
// each, most severe first.
func Findings(ctx context.Context, r store.Reader, repo string, files []string) ([]DeltaFinding, error) {
	out, err := findingsIn(ctx, r, repo, files)
	slices.SortFunc(out, func(a, b DeltaFinding) int {
		return cmp.Or(cmp.Compare(model.SeverityRank(b.Severity), model.SeverityRank(a.Severity)), cmp.Compare(a.Location, b.Location))
	})
	return out, err
}

// findingsIn lists the findings in files with the symbol that owns each.
func findingsIn(ctx context.Context, r store.Reader, repo string, files []string) ([]DeltaFinding, error) {
	var out []DeltaFinding
	for _, file := range files {
		ranked, err := r.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindFinding}, FilePrefix: file}, store.ByName, maxDeltaFindings)
		if err != nil {
			return nil, err
		}
		for _, rk := range ranked {
			n := rk.Node
			if n.File != file {
				continue // a longer path with the same prefix
			}
			owner := file
			hits, err := r.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeHasFinding}, store.In, 1, 1)
			if err != nil {
				return nil, err
			}
			if len(hits) > 0 {
				owner = hits[0].Node.ShortName()
			}
			out = append(out, DeltaFinding{Rule: n.Rule, Severity: n.Severity, Title: n.Name, Owner: owner, Location: n.Location()})
		}
	}
	return out, nil
}

// Headline says the delta in one line, after "this change": "introduces
// 1 high; fixes 2 medium".
func (d SecurityDelta) Headline() string {
	part := func(verb string, by map[string]int) string {
		var bits []string
		for _, sev := range []string{"critical", "high", "medium", "low", "info"} {
			if n := by[sev]; n > 0 {
				bits = append(bits, strconv.Itoa(n)+" "+sev)
			}
		}
		if len(bits) == 0 {
			return ""
		}
		return verb + " " + strings.Join(bits, ", ")
	}
	parts := slices.DeleteFunc([]string{part("introduces", d.Introduced), part("fixes", d.Fixed)}, func(s string) bool { return s == "" })
	if len(parts) == 0 {
		return "introduces and fixes no security findings"
	}
	return strings.Join(parts, "; ")
}
