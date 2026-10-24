package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// MaxNeighbours bounds work and stored evidence per declaration. An
// incomplete neighbourhood can never establish an automatic identity match.
const MaxNeighbours = 64

type neighbourhood struct {
	hash           string
	count, omitted int
	incoming       bool
}

func neighbours(g *model.Graph, anchors map[string]string) map[string]neighbourhood {
	sets := map[string]map[string]bool{}
	omitted := map[string]int{}
	add := func(id, feature string) {
		if n, ok := g.Node(id); !ok || !IsSymbol(n.Kind) {
			return
		}
		if feature == "" {
			return
		}
		if sets[id] == nil {
			sets[id] = map[string]bool{}
		}
		if sets[id][feature] {
			return
		}
		if len(sets[id]) == MaxNeighbours {
			omitted[id]++
			return
		}
		sets[id][feature] = true
	}
	anchor := func(id string) string {
		if stable := anchors[id]; stable != "" {
			return stable
		}
		if n, ok := g.Node(id); ok && n.Kind == model.KindExternal {
			return "external:" + n.ID
		}
		return ""
	}
	for _, e := range g.Edges() {
		if e.From == e.To || e.Kind != model.EdgeCalls && e.Kind != model.EdgeDispatchesTo {
			continue
		}
		if target := anchor(e.To); target != "" {
			add(e.From, "out:"+string(e.Kind)+":"+target)
		}
		if source := anchors[e.From]; source != "" {
			add(e.To, "in:"+string(e.Kind)+":"+source)
		}
	}
	out := map[string]neighbourhood{}
	for id, set := range sets {
		n := neighbourhood{count: len(set), omitted: omitted[id]}
		if n.count >= 2 && n.omitted == 0 {
			features := make([]string, 0, len(set))
			for feature := range set {
				features = append(features, feature)
				if strings.HasPrefix(feature, "in:") {
					n.incoming = true
				}
			}
			slices.Sort(features)
			hash := sha256.Sum256([]byte(strings.Join(features, "\n")))
			n.hash = hex.EncodeToString(hash[:])
		}
		out[id] = n
	}
	return out
}

func (l *Location) setNeighbours(n neighbourhood) {
	l.Neighbourhood = n.hash
	l.NeighbourCount = n.count
	l.NeighboursOmitted = n.omitted
}
