// Package modelpattern holds the model-name patterns of the gateway's local
// exclude list: an exact model name, or a prefix ending in "*"
// (claude-opus-*). Organisations store them in PostgreSQL and operators set
// them in config/llm.yaml; both go through Normalize.
package modelpattern

import (
	"fmt"
	"slices"
	"strings"
)

const (
	// MaxPatterns is the most patterns one list may hold.
	MaxPatterns = 32
	// MaxLen is the longest pattern, "*" included.
	MaxLen = 64
)

// valid says whether p (already lower-cased) is a name of [a-z0-9.:_-] with
// an optional trailing "*". A lone "*" is refused: keeping every model off
// the local model is what switching local first off and dropping the
// gateway provider are for.
func valid(p string) bool {
	name := strings.TrimSuffix(p, "*")
	if name == "" || len(p) > MaxLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != ':' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// Normalize trims, lower-cases, checks, deduplicates and sorts a list. The
// error names the first entry it refuses, cut to MaxLen bytes. Empty entries
// are dropped, so a textarea's trailing newline is harmless. The result is
// never nil.
func Normalize(in []string) ([]string, error) {
	out := make([]string, 0, min(len(in), MaxPatterns+1))
	for _, raw := range in {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			continue
		}
		if !valid(p) {
			shown := p
			if len(shown) > MaxLen {
				shown = shown[:MaxLen]
			}
			return nil, fmt.Errorf("%q is not a model name or a prefix ending in *: use a-z, 0-9, '.', ':', '_' and '-', at most %d characters", shown, MaxLen)
		}
		out = append(out, p)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxPatterns {
		return nil, fmt.Errorf("at most %d models may be excluded, got %d", MaxPatterns, len(out))
	}
	return out, nil
}

// Match says whether model matches one of the normalized patterns: equal to
// a name, or starting with a prefix pattern. Case is ignored.
func Match(patterns []string, model string) bool {
	if len(patterns) == 0 {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(model, prefix) {
				return true
			}
		} else if model == p {
			return true
		}
	}
	return false
}
