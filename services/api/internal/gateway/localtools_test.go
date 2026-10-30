package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const weatherTool = `{"name":"weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}`

// toolServer answers every chat request with the given stream and keeps the
// request body.
func toolServer(t *testing.T, stream string, got *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = string(b)
		_, _ = io.WriteString(w, stream)
	}))
}

func askLocal(t *testing.T, l *Local, body string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	_, _, ok := l.serve(t.Context(), rec, []byte(body))
	return rec, ok
}

func TestLocalToolCallStream(t *testing.T) {
	cases := map[string]struct {
		stream string
		want   []string
		not    []string
	}{
		"one call in fragments": {
			stream: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"weather","arguments":""}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Oslo\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}` + "\n\n" +
				"data: [DONE]\n\n",
			want: []string{
				`"type":"tool_use"`, `"id":"call_1"`, `"name":"weather"`, `"type":"input_json_delta"`,
				`"partial_json":"{\"city\":\"Oslo\"}"`, `"stop_reason":"tool_use"`, `"output_tokens":4`, "event: message_stop",
			},
			not: []string{`"type":"text_delta"`},
		},
		"text then two calls": {
			stream: `data: {"choices":[{"delta":{"content":"Checking."}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"weather","arguments":"{\"city\":\"Oslo\"}"}},{"index":1,"id":"b","function":{"name":"weather","arguments":"{\"city\":\"Rome\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n",
			want: []string{
				`"index":0,"type":"content_block_start"`, `"text":"Checking."`, `"index":1,"type":"content_block_start"`,
				`"id":"a"`, `"index":2,"type":"content_block_start"`, `"id":"b"`, `"stop_reason":"tool_use"`,
			},
		},
		"missing id gets a unique one": {
			stream: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":"{}"}},{"index":1,"function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n",
			want: []string{`"id":"toolu_cs_`},
		},
		"no call, plain text": {
			stream: `data: {"choices":[{"delta":{"content":"No tool needed."},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n",
			want:   []string{`"text":"No tool needed."`, `"stop_reason":"end_turn"`},
			not:    []string{`tool_use`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got string
			srv := toolServer(t, tc.stream, &got)
			defer srv.Close()
			l := &Local{BaseURL: srv.URL, Model: "qwen"}
			rec, ok := askLocal(t, l, `{"model":"claude-haiku-5-5","stream":true,"max_tokens":50,"tools":[`+weatherTool+`],"messages":[{"role":"user","content":"weather?"}]}`)
			require.True(t, ok)
			assert.Equal(t, rec.Header().Get(RouteHeader), "local")
			out := rec.Body.String()
			for _, w := range tc.want {
				assert.True(t, strings.Contains(out, w), w)
			}
			for _, n := range tc.not {
				assert.True(t, !strings.Contains(out, n), n)
			}
			assert.True(t, strings.Contains(got, `"tools":[{"function":{"description":"Get weather"`), "tools reach the local model")
		})
	}
}

func TestLocalToolIDsAreUnique(t *testing.T) {
	var got string
	srv := toolServer(t, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":"{}"}},{"index":1,"function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n"+"data: [DONE]\n\n", &got)
	defer srv.Close()
	rec, ok := askLocal(t, &Local{BaseURL: srv.URL, Model: "q"}, `{"model":"claude-haiku-5-5","tools":[`+weatherTool+`],"messages":[{"role":"user","content":"x"}]}`)
	require.True(t, ok)
	var msg struct {
		Content    []struct{ ID, Type string } `json:"content"`
		StopReason string                      `json:"stop_reason"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &msg))
	require.Equal(t, len(msg.Content), 2)
	assert.True(t, msg.Content[0].ID != "" && msg.Content[0].ID != msg.Content[1].ID)
	assert.Equal(t, msg.StopReason, "tool_use")
}

func TestLocalToolCallNonStream(t *testing.T) {
	var got string
	srv := toolServer(t, `data: {"choices":[{"delta":{"content":"Sure."}}]}`+"\n\n"+
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"weather","arguments":"{\"city\":\"Oslo\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`+"\n\n"+
		"data: [DONE]\n\n", &got)
	defer srv.Close()
	rec, ok := askLocal(t, &Local{BaseURL: srv.URL, Model: "q"}, `{"model":"claude-haiku-5-5","stream":false,"tools":[`+weatherTool+`],"messages":[{"role":"user","content":"x"}]}`)
	require.True(t, ok)
	var msg struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      map[string]int `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &msg))
	require.Equal(t, len(msg.Content), 2)
	assert.Equal(t, msg.Content[0].Type+":"+msg.Content[0].Text, "text:Sure.")
	assert.Equal(t, msg.Content[1].Type+":"+msg.Content[1].ID+":"+msg.Content[1].Name, "tool_use:c1:weather")
	assert.Equal(t, string(msg.Content[1].Input), `{"city":"Oslo"}`)
	assert.Equal(t, msg.StopReason, "tool_use")
	assert.Equal(t, msg.Usage["output_tokens"], 3)
}

func TestLocalToolRoundTripRequest(t *testing.T) {
	var got string
	srv := toolServer(t, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n", &got)
	defer srv.Close()
	body := `{"model":"claude-haiku-5-5","system":"s","tools":[` + weatherTool + `],"messages":[
		{"role":"user","content":"weather in Oslo?"},
		{"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"tu1","name":"weather","input":{"city":"Oslo"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"3C"}]},{"type":"text","text":"thanks"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"tu2","name":"weather","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu2","content":"boom","is_error":true}]}]}`
	_, ok := askLocal(t, &Local{BaseURL: srv.URL, Model: "q"}, body)
	require.True(t, ok)

	var req struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct{ Name, Arguments string }
			} `json:"tool_calls"`
		}
	}
	require.NoError(t, json.Unmarshal([]byte(got), &req))
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	assert.Equal(t, strings.Join(roles, ","), "system,user,assistant,tool,user,assistant,tool")
	assert.Equal(t, req.Messages[2].Content, "Looking.")
	require.Equal(t, len(req.Messages[2].ToolCalls), 1)
	assert.Equal(t, req.Messages[2].ToolCalls[0].ID, "tu1")
	assert.Equal(t, req.Messages[2].ToolCalls[0].Type, "function")
	assert.Equal(t, req.Messages[2].ToolCalls[0].Function.Arguments, `{"city":"Oslo"}`)
	assert.Equal(t, req.Messages[3].ToolCallID+":"+req.Messages[3].Content, "tu1:3C")
	assert.Equal(t, req.Messages[4].Content, "thanks")
	assert.Equal(t, req.Messages[5].ToolCalls[0].Function.Arguments, `{}`)
	assert.Equal(t, req.Messages[6].ToolCallID+":"+req.Messages[6].Content, "tu2:[error] boom")
}

func TestLocalToolChoiceMapping(t *testing.T) {
	cases := map[string]struct {
		choice   string
		want     string
		parallel bool
	}{
		"auto":         {`{"type":"auto"}`, `"tool_choice":"auto"`, true},
		"any":          {`{"type":"any"}`, `"tool_choice":"required"`, true},
		"none":         {`{"type":"none"}`, `"tool_choice":"none"`, true},
		"named":        {`{"type":"tool","name":"weather"}`, `"tool_choice":{"function":{"name":"weather"},"type":"function"}`, true},
		"no parallel":  {`{"type":"auto","disable_parallel_tool_use":true}`, `"parallel_tool_calls":false`, false},
		"unset":        {``, ``, true},
		"unknown type": {`{"type":"weird"}`, ``, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"claude-haiku-5-5","tools":[` + weatherTool + `],"messages":[{"role":"user","content":"x"}]`
			if tc.choice != "" {
				body += `,"tool_choice":` + tc.choice
			}
			l := &Local{}
			_, out, reason := l.translate(t.Context(), []byte(body+`}`))
			if name == "unknown type" {
				assert.Equal(t, reason, FallNotEligible, "unknown tool_choice goes to the cloud")
				return
			}
			require.Equal(t, reason, "")
			require.True(t, out != nil)
			s := string(out)
			if tc.want != "" {
				assert.True(t, strings.Contains(s, tc.want), tc.want)
			}
			assert.Equal(t, strings.Contains(s, "parallel_tool_calls"), !tc.parallel)
			if tc.choice == "" {
				assert.True(t, !strings.Contains(s, "tool_choice"))
			}
		})
	}
}

func TestLocalToolRequestsFallThrough(t *testing.T) {
	good := `{"model":"claude-haiku-5-5","tools":[` + weatherTool + `],"messages":[{"role":"user","content":"x"}]}`
	cases := map[string]struct {
		body   string
		stream string
	}{
		"image block": {
			body: `{"model":"claude-haiku-5-5","tools":[` + weatherTool + `],"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`,
		},
		"image in tool result": {
			body: `{"model":"claude-haiku-5-5","tools":[` + weatherTool + `],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{}}]}]}]}`,
		},
		"server tool": {
			body: `{"model":"claude-haiku-5-5","tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"x"}]}`,
		},
		"tool choice without tools": {
			body: `{"model":"claude-haiku-5-5","tool_choice":{"type":"auto"},"messages":[{"role":"user","content":"x"}]}`,
		},
		"malformed arguments": {
			body:   good,
			stream: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" + "data: [DONE]\n\n",
		},
		"arguments not an object": {
			body:   good,
			stream: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"weather","arguments":"[1]"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" + "data: [DONE]\n\n",
		},
		"undeclared tool": {
			body:   good,
			stream: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"rm_rf","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" + "data: [DONE]\n\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got string
			srv := toolServer(t, tc.stream, &got)
			defer srv.Close()
			rec, ok := askLocal(t, &Local{BaseURL: srv.URL, Model: "q"}, tc.body)
			assert.True(t, !ok, "cloud takes it")
			assert.Equal(t, rec.Body.Len(), 0, "nothing written before the fall-through")
		})
	}
}

// A reasoning model asked for few tokens spends them all on thinking unless
// the provider's settings reach it, and the answer falls through as truncated.
func TestLocalSendsReasoningSettings(t *testing.T) {
	cases := map[string]struct {
		local Local
		want  map[string]any
		not   []string
	}{
		"unset":            {not: []string{"reasoning_effort", "chat_template_kwargs"}},
		"ollama effort":    {local: Local{Effort: "none"}, want: map[string]any{"reasoning_effort": "none"}, not: []string{"chat_template_kwargs"}},
		"vllm no thinking": {local: Local{DisableThinking: true}, want: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}, not: []string{"reasoning_effort"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var got string
			srv := toolServer(t, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n", &got)
			defer srv.Close()
			l := c.local
			l.BaseURL, l.Model = srv.URL, "q"
			_, ok := askLocal(t, &l, `{"model":"claude-haiku-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
			require.True(t, ok)
			var req map[string]any
			require.NoError(t, json.Unmarshal([]byte(got), &req))
			for k, v := range c.want {
				assert.Equal(t, fmt.Sprint(req[k]), fmt.Sprint(v), k)
			}
			for _, k := range c.not {
				_, has := req[k]
				assert.True(t, !has, k+" is not sent")
			}
		})
	}
}

// With local thinking on (the default), a request that asks for thinking or
// carries Claude's thinking blocks goes to the local model without them; off,
// the cloud takes it.
func TestLocalThinking(t *testing.T) {
	asked := `{"model":"claude-opus-5-5","max_tokens":100,"thinking":{"type":"adaptive"},"tools":[` + weatherTool + `],"messages":[
		{"role":"user","content":"weather in Oslo?"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"redacted_thinking","data":"x"},{"type":"tool_use","id":"tu1","name":"weather","input":{"city":"Oslo"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"3C"}]}]}`
	answer := `data: {"choices":[{"delta":{"content":"It is 3C."},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"

	t.Run("on by default", func(t *testing.T) {
		var got string
		srv := toolServer(t, answer, &got)
		defer srv.Close()
		rec, ok := askLocal(t, &Local{BaseURL: srv.URL, Model: "q"}, asked)
		require.True(t, ok)
		assert.True(t, strings.Contains(rec.Body.String(), "It is 3C."))
		assert.True(t, !strings.Contains(got, "sig") && !strings.Contains(got, "thinking"), "thinking stays out of the local request")
		var req struct {
			Messages []struct {
				Role      string `json:"role"`
				ToolCalls []any  `json:"tool_calls"`
			}
		}
		require.NoError(t, json.Unmarshal([]byte(got), &req))
		require.Len(t, req.Messages, 3)
		assert.Equal(t, len(req.Messages[1].ToolCalls), 1, "the tool call around the thinking is kept")
	})
	t.Run("off", func(t *testing.T) {
		var got string
		srv := toolServer(t, answer, &got)
		defer srv.Close()
		l := &Local{BaseURL: srv.URL, Model: "q"}
		for name, body := range map[string]string{
			"asked":     `{"model":"claude-opus-5-5","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"x"}]}`,
			"sent back": strings.Replace(asked, `"thinking":{"type":"adaptive"},`, "", 1),
		} {
			_, out, reason := l.translate(WithLocalThinking(t.Context(), false), []byte(body))
			assert.True(t, out == nil, name)
			assert.Equal(t, reason, FallHasThinking, name)
		}
	})
	t.Run("user thinking blocks stay ineligible", func(t *testing.T) {
		l := &Local{Model: "q"}
		_, out, _ := l.translate(t.Context(), []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"thinking","thinking":"x"}]}]}`))
		assert.True(t, out == nil, "only the assistant's own thinking is left out")
	})
}
