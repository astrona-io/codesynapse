// Package identity keeps symbol identities apart from their current graph
// locators. Matches require independent evidence; uncertain matches stay data
// for review and never transfer an identity.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Bounds prevent unbounded ingest state. Exhaustion fails the ingest rather
// than silently forgetting identities. History omissions are counted.
const (
	MaxSymbols     = 50000
	MaxHistory     = 10
	MaxCandidates  = 20
	MaxReportBytes = 64 << 20
)

// Location is revision-specific evidence, never a permanent identity.
type Location struct {
	NodeID      string `json:"node_id"`
	Name        string `json:"name"`
	File        string `json:"file,omitempty"`
	Signature   string `json:"signature,omitempty"`
	Revision    string `json:"revision,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	CloneHash   string `json:"clone_hash,omitempty"`
	StartLine   int    `json:"start_line,omitempty"`
	EndLine     int    `json:"end_line,omitempty"`
}

// Symbol retains removed symbols too, so a deleted name cannot reclaim its
// old identity just by being introduced again.
type Symbol struct {
	ID                string         `json:"id"`
	Kind              model.NodeKind `json:"kind"`
	Language          string         `json:"language,omitempty"`
	Active            bool           `json:"active"`
	Current           Location       `json:"current"`
	History           []Location     `json:"history"`
	HistoryOmitted    int            `json:"history_omitted"`
	Match             string         `json:"match"`
	Candidates        []string       `json:"candidates"`
	CandidatesOmitted int            `json:"candidates_omitted"`
}

// Report is persisted on the repository node in the same swap as the graph.
type Report struct {
	Version   int      `json:"version"`
	Repo      string   `json:"repo"`
	Revision  string   `json:"revision,omitempty"`
	IndexedAt string   `json:"indexed_at,omitempty"`
	Symbols   []Symbol `json:"symbols"`
}

// IsSymbol identifies declarations supported by the code catalog.
func IsSymbol(k model.NodeKind) bool {
	return k == model.KindPackage || k == model.KindFunction || k == model.KindMethod || k == model.KindInterfaceMethod || k == model.KindType
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand cannot fail on supported platforms
	return "sym-" + hex.EncodeToString(b[:])
}

// Decode refuses a mismatched repository, duplicate IDs or an unsupported
// version. Corrupt prior analysis must not be presented as an empty baseline.
func Decode(raw, repo string) (Report, error) {
	if raw == "" {
		return Report{Version: 1, Repo: repo}, nil
	}
	var r Report
	if len(raw) > MaxReportBytes {
		return Report{}, fmt.Errorf("symbol identity baseline exceeds %d bytes", MaxReportBytes)
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Report{}, fmt.Errorf("symbol identities: %w", err)
	}
	if r.Version != 1 || r.Repo != repo || len(r.Symbols) > MaxSymbols {
		return Report{}, fmt.Errorf("invalid symbol identity baseline for %s", repo)
	}
	seen := map[string]bool{}
	activeLocators := map[string]bool{}
	for _, s := range r.Symbols {
		if len(s.ID) != 36 || !strings.HasPrefix(s.ID, "sym-") || seen[s.ID] || len(s.History) > MaxHistory || len(s.Candidates) > MaxCandidates || !strings.HasPrefix(s.Current.NodeID, repo+"|") || !IsSymbol(s.Kind) || s.HistoryOmitted < 0 || s.CandidatesOmitted < 0 || s.Active && activeLocators[s.Current.NodeID] {
			return Report{}, fmt.Errorf("invalid symbol identity entry")
		}
		if _, err := hex.DecodeString(s.ID[4:]); err != nil {
			return Report{}, fmt.Errorf("invalid permanent symbol ID")
		}
		seen[s.ID] = true
		if s.Active {
			activeLocators[s.Current.NodeID] = true
		}
	}
	return r, nil
}

func location(n model.Node, revision string) Location {
	return Location{NodeID: n.ID, Name: n.Name, File: n.File, Signature: n.Signature, Revision: revision, Fingerprint: n.BodyHash, CloneHash: n.CloneHash, StartLine: n.StartLine, EndLine: n.EndLine}
}

func compatible(s Symbol, n model.Node) bool { return s.Kind == n.Kind && s.Language == n.Language }

// Evidence needs an exact fingerprint or structural fingerprint, plus the
// same name or file. Structure alone abstracts too much to prove identity.
func evidence(s Symbol, n model.Node) bool {
	if !compatible(s, n) {
		return false
	}
	a := s.Current
	content := a.Fingerprint != "" && a.Fingerprint == n.BodyHash
	structure := a.CloneHash != "" && a.CloneHash == n.CloneHash
	signature := strings.ReplaceAll(a.Signature, a.Name, "<name>") == strings.ReplaceAll(n.Signature, n.Name, "<name>")
	return (content || structure && signature) && (a.Name == n.Name || a.File != "" && a.File == n.File)
}

// Reconcile stamps g and returns the next baseline. Graph IDs remain current
// locators for compatibility; SymbolID is the durable identity. Removed
// symbols are never automatically resurrected, nor are copies called moves.
func Reconcile(g *model.Graph, repo string, previous Report) (Report, error) {
	if previous.Repo != repo || previous.Version != 1 {
		return Report{}, fmt.Errorf("symbol identity repository/version mismatch")
	}
	rn, ok := g.Node(model.NodeID(repo, model.KindRepository, repo))
	if !ok {
		return Report{}, fmt.Errorf("symbol identity repository is missing")
	}
	r := Report{Version: 1, Repo: repo, Revision: rn.Head, IndexedAt: rn.Time, Symbols: slices.Clone(previous.Symbols)}
	current := []model.Node{}
	for _, n := range g.Nodes() {
		if n.Repo != repo {
			return Report{}, fmt.Errorf("symbol identity graph crosses repositories")
		}
		if IsSymbol(n.Kind) {
			current = append(current, n)
		}
	}
	if len(current) > MaxSymbols || len(r.Symbols) > MaxSymbols {
		return Report{}, fmt.Errorf("symbol identity limit of %d exceeded", MaxSymbols)
	}
	byLocator := map[string]int{}
	byFingerprint := map[string][]int{}
	byClone := map[string][]int{}
	fingerprintCounts := map[string]int{}
	cloneCounts := map[string]int{}
	for i, s := range previous.Symbols {
		r.Symbols[i].Active = false
		if !s.Active {
			continue
		}
		byLocator[s.Current.NodeID] = i
		if s.Current.Fingerprint != "" {
			fingerprintCounts[s.Current.Fingerprint]++
			if len(byFingerprint[s.Current.Fingerprint]) <= MaxCandidates {
				byFingerprint[s.Current.Fingerprint] = append(byFingerprint[s.Current.Fingerprint], i)
			}
		}
		if s.Current.CloneHash != "" {
			cloneCounts[s.Current.CloneHash]++
			if len(byClone[s.Current.CloneHash]) <= MaxCandidates {
				byClone[s.Current.CloneHash] = append(byClone[s.Current.CloneHash], i)
			}
		}
	}
	assigned := map[int]int{}
	used := map[int]bool{}
	for i, n := range current {
		if old, ok := byLocator[n.ID]; ok && compatible(previous.Symbols[old], n) {
			assigned[i] = old
			used[old] = true
		}
	}
	candidates := map[int][]int{}
	claims := map[int]int{}
	unexamined := map[int]int{}
	for i, n := range current {
		if _, ok := assigned[i]; ok {
			continue
		}
		possibles := slices.Clone(byFingerprint[n.BodyHash])
		possibles = append(possibles, byClone[n.CloneHash]...)
		unexamined[i] = max(0, fingerprintCounts[n.BodyHash]-len(byFingerprint[n.BodyHash])) + max(0, cloneCounts[n.CloneHash]-len(byClone[n.CloneHash]))
		slices.Sort(possibles)
		for _, old := range slices.Compact(possibles) {
			if evidence(previous.Symbols[old], n) {
				candidates[i] = append(candidates[i], old)
				if !used[old] {
					claims[old]++
				}
			}
		}
	}
	for i, n := range current {
		old, exists := assigned[i]
		match := "same declaration"
		if !exists && len(candidates[i]) == 1 && unexamined[i] == 0 {
			candidate := candidates[i][0]
			if !used[candidate] && claims[candidate] == 1 {
				old = candidate
				exists = true
				match = "rename or move: fingerprint and location/name evidence"
			}
		}
		var s Symbol
		if exists {
			s = r.Symbols[old]
			s.History = slices.Clone(s.History)
			next := location(n, r.Revision)
			if s.Current != next {
				s.History = append(s.History, s.Current)
				if len(s.History) > MaxHistory {
					s.History = s.History[1:]
					s.HistoryOmitted++
				}
			}
			s.Current = next
		} else {
			if len(r.Symbols) >= MaxSymbols {
				return Report{}, fmt.Errorf("symbol identity limit of %d exceeded; prior identities retained", MaxSymbols)
			}
			s = Symbol{ID: newID(), Kind: n.Kind, Language: n.Language, Current: location(n, r.Revision), History: []Location{}}
			old = len(r.Symbols)
			r.Symbols = append(r.Symbols, s)
			match = "new declaration"
		}
		s.Active = true
		// Preserve unresolved provenance until an explicit review, including
		// on an unchanged re-ingest. It is never silently promoted to fact.
		if (len(candidates[i]) > 0 || unexamined[i] > 0) && !exists {
			s.Candidates = []string{}
			s.CandidatesOmitted = unexamined[i]
			for _, candidate := range candidates[i] {
				s.Candidates = append(s.Candidates, previous.Symbols[candidate].ID)
			}
			match = "ambiguous identity or possible copy; review required"
			if len(s.Candidates) > MaxCandidates {
				s.CandidatesOmitted += len(s.Candidates) - MaxCandidates
				s.Candidates = s.Candidates[:MaxCandidates]
			}
		} else if len(s.Candidates) > 0 || s.CandidatesOmitted > 0 {
			match = s.Match
		}
		s.Match = match
		r.Symbols[old] = s
		stored, _ := g.Node(n.ID)
		stored.SymbolID = s.ID
		stored.Revision = r.Revision
	}
	return r, nil
}

// Encode serializes the bounded report for the repository node.
func Encode(r Report) (string, error) {
	b, err := json.Marshal(r)
	if len(b) > MaxReportBytes {
		return "", fmt.Errorf("symbol identity report exceeds %d bytes; prior identities retained", MaxReportBytes)
	}
	return string(b), err
}
