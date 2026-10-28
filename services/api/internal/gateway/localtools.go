package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// maxCollected bounds what the gateway holds of one held answer (tool-using,
// or for a bigger model than the small one) before it checks it; a runaway
// local model goes to the cloud instead.
const maxCollected = 1 << 20

// oaMsg is one OpenAI chat message.
type oaMsg struct {
	Role       string   `json:"role"`
	Content    string   `json:"content"`
	ToolCalls  []oaCall `json:"tool_calls,omitempty"`
	ToolCallID string   `json:"tool_call_id,omitempty"`
}

type oaCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// convertMessage turns one Anthropic message into OpenAI messages. Images,
// thinking and any other block make the request ineligible.
func convertMessage(m anthropicMsg) ([]oaMsg, bool) {
	if m.Role != "user" && m.Role != "assistant" {
		return nil, false
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []oaMsg{{Role: m.Role, Content: s}}, true
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil, false
	}
	var texts []string
	var results []oaMsg
	var uses []oaCall
	for _, b := range blocks {
		switch {
		case b.Type == "text":
			texts = append(texts, b.Text)
		case b.Type == "tool_use" && m.Role == "assistant":
			args := strings.TrimSpace(string(b.Input))
			if args == "" || args == "null" {
				args = "{}"
			}
			c := oaCall{ID: b.ID, Type: "function"}
			c.Function.Name, c.Function.Arguments = b.Name, args
			uses = append(uses, c)
		case b.Type == "tool_result" && m.Role == "user":
			t, ok := resultText(b.Content)
			if !ok {
				return nil, false
			}
			if b.IsError {
				t = "[error] " + t
			}
			results = append(results, oaMsg{Role: "tool", ToolCallID: b.ToolUseID, Content: t})
		default:
			return nil, false
		}
	}
	joined := strings.Join(texts, "\n\n")
	if m.Role == "assistant" {
		return []oaMsg{{Role: "assistant", Content: joined, ToolCalls: uses}}, true
	}
	// Results come first: OpenAI wants them straight after the assistant's calls.
	out := results
	if len(texts) > 0 || len(results) == 0 {
		out = append(out, oaMsg{Role: "user", Content: joined})
	}
	return out, true
}

// resultText flattens a tool result: a string or text blocks.
func resultText(raw json.RawMessage) (string, bool) {
	if !present(raw) {
		return "", true
	}
	return text(raw)
}

// customTool is the part of an Anthropic tool definition the gateway needs.
type customTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// convertTools maps tools and tool_choice. Server tools (web search, code
// execution) run at the provider and make the request ineligible.
func convertTools(raw []json.RawMessage, choice json.RawMessage) (tools []map[string]any, oaChoice any, parallel, ok bool) {
	if len(raw) == 0 {
		return nil, nil, true, !present(choice)
	}
	for _, r := range raw {
		var t customTool
		if json.Unmarshal(r, &t) != nil || t.Name == "" || (t.Type != "" && t.Type != "custom") {
			return nil, nil, true, false
		}
		params := t.InputSchema
		if !present(params) {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": params,
		}})
	}
	if !present(choice) {
		return tools, nil, true, true
	}
	var c struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if json.Unmarshal(choice, &c) != nil {
		return nil, nil, true, false
	}
	parallel = !c.DisableParallelToolUse
	switch c.Type {
	case "auto":
		oaChoice = "auto"
	case "any":
		oaChoice = "required"
	case "none":
		oaChoice = "none"
	case "tool":
		if c.Name == "" {
			return nil, nil, true, false
		}
		oaChoice = map[string]any{"type": "function", "function": map[string]string{"name": c.Name}}
	default:
		return nil, nil, true, false
	}
	return tools, oaChoice, parallel, true
}

// declaredTools lists the names the request offered.
func declaredTools(raw []json.RawMessage) map[string]bool {
	names := map[string]bool{}
	for _, r := range raw {
		var t customTool
		if json.Unmarshal(r, &t) == nil {
			names[t.Name] = true
		}
	}
	return names
}

type toolCall struct {
	ID, Name, Args string
}

// answer is a whole local answer, text plus tool calls.
type answer struct {
	Text   string
	Calls  []toolCall
	Finish string
	In     int
	Out    int
}

// complete says the answer ended on its own with something in it: not cut
// off by the token limit and not a stream that stopped without a reason.
func (a answer) complete() bool {
	return a.Finish != "" && a.Finish != "length" && (strings.TrimSpace(a.Text) != "" || len(a.Calls) > 0)
}

func (a answer) stopReason() string {
	switch {
	case len(a.Calls) > 0:
		return "tool_use"
	case a.Finish == "length":
		return "max_tokens"
	}
	return "end_turn"
}

// collect reads the whole answer. ok is false when it is not usable: a call
// to a tool the request did not offer, arguments that are not a JSON object,
// or an answer past maxCollected.
func collect(first chunk, more bool, next func() (chunk, bool), allowed map[string]bool) (answer, bool) {
	var a answer
	var text strings.Builder
	var calls []*toolCall
	at := map[int]*toolCall{}
	size := 0
	for c, ok := first, more; ok; c, ok = next() {
		text.WriteString(c.Text)
		size += len(c.Text)
		for _, d := range c.Calls {
			cur := at[d.Index]
			// A new id on a known index starts another call (some servers
			// reuse one index for every call).
			if cur == nil || (d.ID != "" && cur.ID != "" && d.ID != cur.ID) {
				cur = &toolCall{}
				at[d.Index] = cur
				calls = append(calls, cur)
			}
			if d.ID != "" {
				cur.ID = d.ID
			}
			cur.Name += d.Name
			cur.Args += d.Args
			size += len(d.Name) + len(d.Args)
		}
		if size > maxCollected {
			return a, false
		}
		if c.Finish != "" {
			a.Finish = c.Finish
		}
		if c.In > 0 || c.Out > 0 {
			a.In, a.Out = c.In, c.Out
		}
	}
	a.Text = text.String()
	seen := map[string]bool{}
	for _, c := range calls {
		if !allowed[c.Name] {
			return a, false
		}
		args := strings.TrimSpace(c.Args)
		if args == "" {
			args = "{}"
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(args), &obj) != nil || obj == nil {
			return a, false
		}
		id := c.ID
		if id == "" || seen[id] {
			id = toolID()
		}
		seen[id] = true
		a.Calls = append(a.Calls, toolCall{ID: id, Name: c.Name, Args: args})
	}
	return a, true
}

func toolID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "toolu_cs_" + hex.EncodeToString(b)
}

// streamCollected writes the Anthropic event stream for a collected answer.
func streamCollected(w http.ResponseWriter, model string, a answer) Usage {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": messageID(), "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})
	idx := 0
	if a.Text != "" || len(a.Calls) == 0 {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]string{"type": "text", "text": ""}})
		if a.Text != "" {
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]string{"type": "text_delta", "text": a.Text}})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	for _, c := range a.Calls {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": map[string]any{}}})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]string{"type": "input_json_delta", "partial_json": c.Args}})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": a.stopReason(), "stop_sequence": nil}, "usage": map[string]int{"input_tokens": a.In, "output_tokens": a.Out}})
	send("message_stop", map[string]string{"type": "message_stop"})
	return Usage{In: a.In, Out: a.Out}
}

// jsonCollected writes the single Anthropic message for a collected answer.
func jsonCollected(w http.ResponseWriter, model string, a answer) Usage {
	content := []any{}
	if a.Text != "" || len(a.Calls) == 0 {
		content = append(content, map[string]string{"type": "text", "text": a.Text})
	}
	for _, c := range a.Calls {
		content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": json.RawMessage(c.Args)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": messageID(), "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": a.stopReason(), "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": a.In, "output_tokens": a.Out},
	})
	return Usage{In: a.In, Out: a.Out}
}
