package agentctx

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Response budget limits use ceil(JSON bytes/4), not a model tokenizer.
const (
	MinContextTokens = 256
	MaxContextTokens = 20000
	JSONEstimator    = "ceil(json_bytes/4)"
)

// ErrResponseBudget means even the subject and response metadata do not fit.
var ErrResponseBudget = errors.New("context does not fit the response budget; increase max_tokens or narrow the request")

// ResponseBudget describes the complete structured JSON, including this field.
// Transport framing and tool definitions are outside this estimate. Answers
// carry it only when something was omitted: without it, an answer is
// complete and within max_tokens.
type ResponseBudget struct {
	MaxTokens  int    `json:"max_tokens"`
	UsedTokens int    `json:"used_tokens"`
	Estimator  string `json:"estimator"`
	Source     string `json:"source"`
	Omitted    int    `json:"omitted"`
	Next       string `json:"next,omitempty"`
}

func contextBudget(requested, fallback int) int {
	if requested <= 0 {
		requested = fallback
	}
	return min(max(requested, MinContextTokens), MaxContextTokens)
}

func measuredTokens(value any, used *int) (int, error) {
	// The estimate is part of the payload. Iteration accounts for changes in
	// its decimal width as well as escaped strings and the JSON envelope.
	*used = 0
	for {
		data, err := json.Marshal(value)
		if err != nil {
			return 0, fmt.Errorf("encode context: %w", err)
		}
		n := (len(data) + 3) / 4
		if *used == n {
			return n, nil
		}
		*used = n
	}
}

func fitResponse(value any, out **ResponseBudget, requested int, trim func() int) error {
	// Measured with the block in place, so dropping it only shrinks the answer.
	budget := &ResponseBudget{MaxTokens: contextBudget(requested, MaxContextTokens), Estimator: JSONEstimator, Source: "indexed_graph"}
	*out = budget
	for {
		n, err := measuredTokens(value, &budget.UsedTokens)
		if err != nil {
			return err
		}
		if n <= budget.MaxTokens {
			if budget.Omitted == 0 {
				*out = nil
			}
			return nil
		}
		removed := trim()
		if removed == 0 {
			return ErrResponseBudget
		}
		budget.Omitted += removed
		budget.Next = "Evidence was omitted; narrow the request or increase max_tokens before drawing conclusions."
	}
}

// Halving avoids repeatedly serializing a large report once per removed row.
func trimTail[T any](items *[]T) int {
	n := len(*items)
	if n == 0 {
		return 0
	}
	keep := n / 2
	*items = (*items)[:keep]
	return n - keep
}

func trimSymbol(s *Symbol) int {
	if s.Summary != "" {
		s.Summary = ""
		return 1
	}
	if s.Signature != "" {
		s.Signature = ""
		return 1
	}
	return trimTail(&s.Tags)
}

func finishSymbol(out SymbolContext, maxTokens int) (SymbolContext, error) {
	err := fitResponse(&out, &out.Budget, maxTokens, func() int {
		if out.Churn != nil {
			out.Churn = nil
			return 1
		}
		if out.Duplicate != nil {
			out.Duplicate = nil
			return 1
		}
		for _, refs := range []*[]Ref{&out.Docs, &out.UntestedCallers, &out.Implementations, &out.Callees, &out.Callers, &out.Tests, &out.Findings} {
			if n := trimTail(refs); n > 0 {
				return n
			}
		}
		if p := out.Package; p != nil {
			for _, refs := range []*[]Ref{&p.Hubs, &p.Exported, &p.Interfaces, &p.EntryPoints} {
				if n := trimTail(refs); n > 0 {
					return n
				}
			}
			for _, names := range []*[]string{&p.Imports, &p.ImportedBy} {
				if n := trimTail(names); n > 0 {
					return n
				}
			}
			if p.Summary != "" {
				p.Summary = ""
				return 1
			}
		}
		if n := trimSymbol(&out.Symbol); n > 0 {
			return n
		}
		if out.Risk != nil {
			return trimTail(&out.Risk.Reasons)
		}
		return 0
	})
	if err != nil {
		return SymbolContext{}, err
	}
	return out, nil
}

func finishCallGraph(out CallGraph, maxTokens int) (CallGraph, error) {
	err := fitResponse(&out, &out.Budget, maxTokens, func() int {
		if n := trimTail(&out.Edges); n > 0 {
			return n
		}
		if n := trimTail(&out.Entries); n > 0 {
			return n
		}
		if n := trimTail(&out.Nodes); n > 0 {
			return n
		}
		return trimSymbol(&out.Root)
	})
	if err != nil {
		return CallGraph{}, err
	}
	return out, nil
}

func finishPR(out PRContext, maxTokens int) (PRContext, error) {
	err := fitResponse(&out, &out.Budget, maxTokens, func() int {
		if n := trimTail(&out.Docs); n > 0 {
			return n
		}
		if n := trimTail(&out.Impacted); n > 0 {
			return n
		}
		// The counts of new and fixed findings always stay; their lists go,
		// fixed ones first.
		if d := out.Security; d != nil {
			if n := trimTail(&d.Removed); n > 0 {
				return n
			}
		}
		for _, names := range []*[]string{&out.DuplicatedChanged, &out.ExportedChanged, &out.ExportedUntested, &out.SensitiveTouched, &out.Checklist} {
			if n := trimTail(names); n > 0 {
				return n
			}
		}
		if n := trimTail(&out.Files); n > 0 {
			return n
		}
		if d := out.Security; d != nil {
			if n := trimTail(&d.New); n > 0 {
				return n
			}
		}
		// Changes are already ranked by risk. Keep the highest risk subjects
		// while shedding verbose detail before dropping whole changed symbols.
		removed := 0
		for i := range out.Changes {
			c := &out.Changes[i]
			removed += trimTail(&c.Tests) + trimTail(&c.Findings) + trimTail(&c.Reasons)
			if c.Duplicate != nil {
				c.Duplicate = nil
				removed++
			}
			if c.Signature != "" || c.Summary != "" {
				c.Signature, c.Summary = "", ""
				removed++
			}
		}
		if removed > 0 {
			return removed
		}
		return trimTail(&out.Changes)
	})
	if err != nil {
		return PRContext{}, err
	}
	return out, nil
}
