package memory

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"unicode"
)

// Lexical ranking and fusion for recall (docs/adr/0001-search.md). Entity
// documents are short (name, aliases, type) and a namespace holds at most
// MaxEntitiesPerRepo of them, so BM25 runs in-process over the namespace a
// store has already read through its tenant-rooted lookup. Every store
// ranks with this one function, so they answer alike.

// BM25 parameters: the usual k1, and a lower b because names are short and
// their lengths vary little.
const (
	bm25K1 = 1.2
	bm25B  = 0.5
	// rrfK damps the head of each list in reciprocal rank fusion; 60 is the
	// value from the original paper and what Milvus' RRFRanker defaults to.
	rrfK = 60
	// A query term matches a longer document token by prefix ("pay" finds
	// "payments") from this length on, at half weight.
	minPrefix = 3
)

// Tokens splits text into lower-case search tokens the way Words splits a
// query (so "user_1" matches itself), then adds the parts of each token
// split on - and _ and on camelCase boundaries ("PaymentsAPI" gives
// paymentsapi, payments, api).
func Tokens(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	}) {
		out = append(out, strings.ToLower(f))
		var parts []string
		for _, w := range strings.FieldsFunc(f, func(r rune) bool { return r == '-' || r == '_' }) {
			parts = append(parts, camelParts(w)...)
		}
		if len(parts) > 1 {
			for _, p := range parts {
				out = append(out, strings.ToLower(p))
			}
		}
	}
	return out
}

func camelParts(s string) []string {
	rs := []rune(s)
	var parts []string
	start := 0
	for i := 1; i < len(rs); i++ {
		prev, cur := rs[i-1], rs[i]
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		// aB, or the B in ABc (an acronym followed by a word), or a digit edge.
		if unicode.IsLower(prev) && unicode.IsUpper(cur) ||
			unicode.IsUpper(prev) && unicode.IsUpper(cur) && unicode.IsLower(next) ||
			unicode.IsDigit(prev) != unicode.IsDigit(cur) {
			parts = append(parts, string(rs[start:i]))
			start = i
		}
	}
	return append(parts, string(rs[start:]))
}

func entityTokens(e Entity) []string {
	toks := Tokens(e.Name)
	for _, a := range e.Aliases {
		toks = append(toks, Tokens(a)...)
	}
	if e.Type != "" && e.Type != "Thing" {
		toks = append(toks, strings.ToLower(e.Type))
	}
	return toks
}

// RankBM25 scores entities against query with BM25 and returns the matches,
// best first, at most k. Entities sharing no term with the query are left
// out; an empty or stopword-only query matches nothing.
func RankBM25(entities []Entity, query string, k int) []Scored {
	terms := Words(query)
	out := []Scored{}
	if len(terms) == 0 || len(entities) == 0 {
		return out
	}
	docs := make([][]string, len(entities))
	total := 0
	for i, e := range entities {
		docs[i] = entityTokens(e)
		total += len(docs[i])
	}
	avg := float64(total) / float64(len(docs))
	// match returns how strongly a document token matches a query term.
	match := func(term, tok string) float64 {
		switch {
		case tok == term:
			return 1
		case len([]rune(term)) >= minPrefix && strings.HasPrefix(tok, term):
			return 0.5
		}
		return 0
	}
	tf := make([]map[string]float64, len(docs))
	df := map[string]int{}
	for i, toks := range docs {
		tf[i] = map[string]float64{}
		for _, term := range terms {
			for _, tok := range toks {
				tf[i][term] += match(term, tok)
			}
			if tf[i][term] > 0 {
				df[term]++
			}
		}
	}
	n := float64(len(docs))
	for i, e := range entities {
		score := 0.0
		for _, term := range terms {
			f := tf[i][term]
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[term])+0.5)/(float64(df[term])+0.5))
			norm := bm25K1 * (1 - bm25B + bm25B*float64(len(docs[i]))/avg)
			score += idf * f * (bm25K1 + 1) / (f + norm)
		}
		if score > 0 {
			out = append(out, Scored{Entity: e, Score: score})
		}
	}
	slices.SortFunc(out, func(a, b Scored) int { return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID)) })
	return out[:min(len(out), max(k, 0))]
}

// Fused is one entity after reciprocal rank fusion.
type Fused struct {
	Scored
	// Legs names the ranked lists it appeared in, in the order given.
	Legs []string
}

// FuseRRF merges ranked lists by reciprocal rank fusion: each entity scores
// the sum of 1/(rrfK+rank) over the lists it is in. Ranks, not raw scores,
// are combined, so BM25 scores and cosine similarities need no common
// scale. Entities appear once (deduplicated by ID), best first, at most k.
func FuseRRF(k int, legs map[string][]Scored) []Fused {
	names := make([]string, 0, len(legs))
	for name := range legs {
		names = append(names, name)
	}
	slices.Sort(names) // deterministic Legs order
	byID := map[string]*Fused{}
	for _, name := range names {
		for rank, s := range legs[name] {
			f, ok := byID[s.ID]
			if !ok {
				f = &Fused{Scored: Scored{Entity: s.Entity}}
				byID[s.ID] = f
			}
			if slices.Contains(f.Legs, name) {
				continue // a list naming an entity twice counts once
			}
			f.Score += 1 / float64(rrfK+rank+1)
			f.Legs = append(f.Legs, name)
		}
	}
	out := make([]Fused, 0, len(byID))
	for _, f := range byID {
		out = append(out, *f)
	}
	slices.SortFunc(out, func(a, b Fused) int { return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID)) })
	return out[:min(len(out), max(k, 0))]
}
