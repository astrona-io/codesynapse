package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// Each check translate makes names its own reason, so the statistics say
// which part of a request kept it in the cloud.
func TestTranslateReasons(t *testing.T) {
	const user = `"messages":[{"role":"user","content":"x"}]`
	cases := map[string]struct {
		body string
		want string
	}{
		"bad json":               {`{"model":`, FallBadRequest},
		"no messages":            {`{"model":"claude-haiku-5-5","messages":[]}`, FallBadRequest},
		"unknown role":           {`{"model":"claude-haiku-5-5","messages":[{"role":"system","content":"x"}]}`, FallBadRequest},
		"content not text":       {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":42}]}`, FallBadRequest},
		"model not eligible":     {`{"model":"claude-opus-5-5",` + user + `}`, FallNotEligible},
		"system with a non-text": {`{"model":"claude-haiku-5-5","system":[{"type":"image"}],` + user + `}`, FallSystemNotText},
		"system a number":        {`{"model":"claude-haiku-5-5","system":7,` + user + `}`, FallSystemNotText},
		"server tool use in history": {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"x"},
			{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{}}]},
			{"role":"user","content":"y"}]}`, FallUnsupportedBlock},
		"web search result":              {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"web_search_tool_result","tool_use_id":"s","content":[]}]}]}`, FallUnsupportedBlock},
		"unknown block in a result":      {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"widget"}]}]}]}`, FallUnsupportedBlock},
		"tool_reference without name":    {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"tool_reference"}]}]}]}`, FallUnsupportedBlock},
		"tool_use from the user":         {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"tool_use","id":"t","name":"x","input":{}}]}]}`, FallUnsupportedBlock},
		"web search server tool":         {`{"model":"claude-haiku-5-5","tools":[{"type":"web_search_20250305","name":"web_search"}],` + user + `}`, FallUnsupportedTool},
		"web fetch server tool":          {`{"model":"claude-haiku-5-5","tools":[` + weatherTool + `,{"type":"web_fetch_20250910","name":"web_fetch"}],` + user + `}`, FallUnsupportedTool},
		"tool search server tool":        {`{"model":"claude-haiku-5-5","tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}],` + user + `}`, FallUnsupportedTool},
		"bash client tool":               {`{"model":"claude-haiku-5-5","tools":[{"type":"bash_20250124","name":"bash"}],` + user + `}`, FallUnsupportedTool},
		"tool without a name":            {`{"model":"claude-haiku-5-5","tools":[{"input_schema":{}}],` + user + `}`, FallUnsupportedTool},
		"any without tools":              {`{"model":"claude-haiku-5-5","tool_choice":{"type":"any"},` + user + `}`, FallUnsupportedTool},
		"named tool not offered":         {`{"model":"claude-haiku-5-5","tools":[` + weatherTool + `],"tool_choice":{"type":"tool","name":"other"},` + user + `}`, FallUnsupportedTool},
		"named deferred tool not loaded": {`{"model":"claude-haiku-5-5","tools":[{"name":"later","input_schema":{},"defer_loading":true}],"tool_choice":{"type":"tool","name":"later"},` + user + `}`, FallUnsupportedTool},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, out, reason := (&Local{}).translate(WithLocalFirst(t.Context(), false), []byte(tc.body))
			assert.Equal(t, reason, tc.want)
			assert.True(t, out == nil, "nothing to send")
		})
	}
}

type oaReq struct {
	Messages []oaMsg `json:"messages"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
}

// Request features Claude Code sends that the local model can take as text.
func TestTranslateConverts(t *testing.T) {
	deferred := `{"name":"later","description":"d","input_schema":{"type":"object"},"defer_loading":true}`
	cases := map[string]struct {
		body  string
		check func(t *testing.T, r oaReq)
	}{
		"system text blocks with cache_control": {
			body: `{"model":"claude-haiku-5-5","system":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`,
			check: func(t *testing.T, r oaReq) {
				require.Equal(t, len(r.Messages), 2)
				assert.Equal(t, r.Messages[0].Role+":"+r.Messages[0].Content, "system:a\n\nb")
				assert.Equal(t, r.Messages[1].Content, "x")
			},
		},
		"deferred tool left out until loaded": {
			body: `{"model":"claude-haiku-5-5","tools":[` + weatherTool + `,` + deferred + `],"messages":[{"role":"user","content":"x"}]}`,
			check: func(t *testing.T, r oaReq) {
				require.Equal(t, len(r.Tools), 1)
				assert.Equal(t, r.Tools[0].Function.Name, "weather")
			},
		},
		"tool_reference loads a deferred tool": {
			body: `{"model":"claude-haiku-5-5","tools":[` + weatherTool + `,` + deferred + `],"messages":[
				{"role":"user","content":"x"},
				{"role":"assistant","content":[{"type":"tool_use","id":"ts","name":"ToolSearch","input":{"query":"later"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"ts","content":[{"type":"tool_reference","tool_name":"later"}]}]}]}`,
			check: func(t *testing.T, r oaReq) {
				require.Equal(t, len(r.Tools), 2)
				assert.Equal(t, r.Tools[1].Function.Name, "later")
				last := r.Messages[len(r.Messages)-1]
				assert.Equal(t, last.Role+":"+last.ToolCallID+":"+last.Content, "tool:ts:Tool loaded: later")
			},
		},
		"tool result with text and search results": {
			body: `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[
				{"type":"text","text":"found"},
				{"type":"search_result","title":"Doc","source":"https://example.org/d","content":[{"type":"text","text":"body"}],"citations":{"enabled":true}}]}]}]}`,
			check: func(t *testing.T, r oaReq) {
				require.Equal(t, len(r.Messages), 1)
				assert.Equal(t, r.Messages[0].Content, "found\n\nDoc (https://example.org/d)\nbody")
			},
		},
		"search result in user content": {
			body: `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"search_result","title":"T","source":"s","content":[{"type":"text","text":"b"}]},{"type":"text","text":"q"}]}]}`,
			check: func(t *testing.T, r oaReq) {
				require.Equal(t, len(r.Messages), 1)
				assert.Equal(t, r.Messages[0].Content, "T (s)\nb\n\nq")
			},
		},
		"empty tool result": {
			body: `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t"}]}]}`,
			check: func(t *testing.T, r oaReq) {
				require.Equal(t, len(r.Messages), 1)
				assert.Equal(t, r.Messages[0].Role+":"+r.Messages[0].Content, "tool:")
			},
		},
		"auto without tools": {
			body: `{"model":"claude-haiku-5-5","tool_choice":{"type":"auto"},"messages":[{"role":"user","content":"x"}]}`,
			check: func(t *testing.T, r oaReq) {
				assert.Equal(t, len(r.Tools), 0)
				assert.True(t, len(r.ToolChoice) == 0, "no tool_choice without tools")
			},
		},
		"none without tools": {
			body: `{"model":"claude-haiku-5-5","tool_choice":{"type":"none"},"messages":[{"role":"user","content":"x"}]}`,
			check: func(t *testing.T, r oaReq) {
				assert.Equal(t, len(r.Tools), 0)
			},
		},
		"named loaded deferred tool": {
			body: `{"model":"claude-haiku-5-5","tools":[` + deferred + `],"tool_choice":{"type":"tool","name":"later"},"messages":[
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"ts","content":[{"type":"tool_reference","tool_name":"later"}]}]}]}`,
			check: func(t *testing.T, r oaReq) {
				require.Equal(t, len(r.Tools), 1)
				assert.True(t, strings.Contains(string(r.ToolChoice), `"later"`))
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, out, reason := (&Local{}).translate(t.Context(), []byte(tc.body))
			require.Equal(t, reason, "")
			var r oaReq
			require.NoError(t, json.Unmarshal(out, &r))
			tc.check(t, r)
		})
	}
}

// A local answer may call only the tools the local model was offered: a
// deferred tool nobody loaded is not one of them.
func TestLocalRefusesCallToUnloadedDeferredTool(t *testing.T) {
	var got string
	srv := toolServer(t, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"later","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n"+"data: [DONE]\n\n", &got)
	defer srv.Close()
	body := `{"model":"claude-haiku-5-5","tools":[` + weatherTool + `,{"name":"later","input_schema":{"type":"object"},"defer_loading":true}],"messages":[{"role":"user","content":"x"}]}`
	rec, ok := askLocal(t, &Local{BaseURL: srv.URL, Model: "q"}, body)
	assert.True(t, !ok, "cloud takes it")
	assert.Equal(t, rec.Body.Len(), 0)
	assert.True(t, !strings.Contains(got, `"later"`), "deferred tool not sent to the local model")
}
