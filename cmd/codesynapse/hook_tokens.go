package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"

	"github.com/astrona-io/codesynapse/internal/usage"
)

const maxTokenTranscript = 64 << 20

// Only numeric usage is decoded. Transcript text and message IDs never leave
// this process. Duplicate assistant blocks count as one model response.
func readTokenCounts(path string) (usage.TokenCounts, bool) {
	f, err := os.Open(path) //nolint:gosec // local hook-provided transcript; only bounded numeric counters are reported
	if err != nil {
		return usage.TokenCounts{}, false
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxTokenTranscript {
		return usage.TokenCounts{}, false
	}
	return scanTokenCounts(f)
}

func scanTokenCounts(r io.Reader) (usage.TokenCounts, bool) {
	scan := bufio.NewScanner(io.LimitReader(r, maxTokenTranscript+1))
	scan.Buffer(make([]byte, 4096), 2<<20)
	messages := map[string]usage.TokenCounts{}
	var codex usage.TokenCounts
	seenCodex := false
	for scan.Scan() {
		var event struct {
			Type    string `json:"type"`
			Message struct {
				ID    string `json:"id"`
				Usage *struct {
					Input  int `json:"input_tokens"`
					Output int `json:"output_tokens"`
					Read   int `json:"cache_read_input_tokens"`
					Write  int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Payload struct {
				Type string `json:"type"`
				Info *struct {
					Total *struct {
						Input  int `json:"input_tokens"`
						Output int `json:"output_tokens"`
						Cached int `json:"cached_input_tokens"`
					} `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(scan.Bytes(), &event) != nil {
			continue // unrelated or incomplete transcript events
		}
		if u := event.Message.Usage; event.Type == "assistant" && u != nil && event.Message.ID != "" && len(event.Message.ID) <= 200 {
			t := usage.TokenCounts{Input: u.Input, Output: u.Output, CachedInput: u.Read, Turns: 1}
			if u.Input < 0 || u.Output < 0 || u.Read < 0 || u.Write < 0 || u.Input > usage.MaxHookTokens || u.Output > usage.MaxHookTokens || u.Read > usage.MaxHookTokens || u.Write > usage.MaxHookTokens {
				return usage.TokenCounts{}, false
			}
			// Claude reports uncached input, cache reads and cache writes separately.
			t.Input += u.Read + u.Write
			if t.Validate() != nil {
				return usage.TokenCounts{}, false
			}
			old := messages[event.Message.ID]
			t.Input, t.Output, t.CachedInput = max(t.Input, old.Input), max(t.Output, old.Output), max(t.CachedInput, old.CachedInput)
			messages[event.Message.ID] = t
			if len(messages) > 100_000 {
				return usage.TokenCounts{}, false
			}
		}
		if event.Type == "event_msg" && event.Payload.Type == "token_count" && event.Payload.Info != nil && event.Payload.Info.Total != nil {
			u := event.Payload.Info.Total
			t := usage.TokenCounts{Input: u.Input, Output: u.Output, CachedInput: u.Cached}
			if t.Validate() != nil {
				return usage.TokenCounts{}, false
			}
			codex.Input, codex.Output, codex.CachedInput = max(codex.Input, t.Input), max(codex.Output, t.Output), max(codex.CachedInput, t.CachedInput)
			seenCodex = true
		}
	}
	if scan.Err() != nil {
		return usage.TokenCounts{}, false
	}
	if seenCodex {
		return codex, true
	}
	var total usage.TokenCounts
	for _, t := range messages {
		total.Input += t.Input
		total.Output += t.Output
		total.CachedInput += t.CachedInput
		total.Turns++
	}
	return total, len(messages) > 0 && total.Validate() == nil
}

func hookTokenDelta(path string, sent usage.TokenCounts) (usage.TokenCounts, *usage.TokenCounts) {
	total, ok := readTokenCounts(path)
	if !ok {
		return total, nil
	}
	delta := usage.TokenCounts{Input: max(0, total.Input-sent.Input), Output: max(0, total.Output-sent.Output),
		CachedInput: max(0, total.CachedInput-sent.CachedInput), Turns: max(0, total.Turns-sent.Turns)}
	delta.CachedInput = min(delta.CachedInput, delta.Input)
	if delta == (usage.TokenCounts{}) || delta.Validate() != nil {
		return total, nil
	}
	// Keep a high-water mark if the transcript was compacted or truncated.
	total.Input, total.Output = max(total.Input, sent.Input), max(total.Output, sent.Output)
	total.CachedInput, total.Turns = max(total.CachedInput, sent.CachedInput), max(total.Turns, sent.Turns)
	return total, &delta
}
