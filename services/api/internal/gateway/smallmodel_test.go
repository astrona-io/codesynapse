package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestSmallModel(t *testing.T) {
	cases := map[string]bool{
		"claude-haiku-5-5":                                 true,
		"claude-haiku-5-5-20261001":                        true,
		"Claude-Haiku-5-5":                                 true,
		"claude-haiku-5-5[1m]":                             true,
		"us.anthropic.claude-haiku-5-5-20261001-v1:0":      true,
		"anthropic.claude-haiku-5-5-20261001-v1:0":         true,
		"claude-haiku-5-5@20261001":                        true,
		"claude-haiku-4-5":                                 true,
		"claude-3-5-haiku-20241022":                        true,
		"claude-3-haiku-20240307":                          true,
		"haiku":                                            true,
		"claude-opus-5-5":                                  false,
		"claude-sonnet-5-5-20261001":                       false,
		"claude-fable-5-1":                                 false,
		"us.anthropic.claude-sonnet-5-5-20261001-v1:0[1m]": false,
		"haiku-7b":                                         false,
		"qwen-haiku-distill":                               false,
		"claude-haikus":                                    false,
		"claude-sonnet-haiku-merge":                        false,
		"gpt-5":                                            false,
		"":                                                 false,
	}
	for model, want := range cases {
		t.Run(model, func(t *testing.T) {
			assert.Equal(t, SmallModel(model), want)
		})
	}
}

func TestQuick(t *testing.T) {
	tool := json.RawMessage(`{"name":"Bash","input_schema":{"type":"object"}}`)
	cases := map[string]struct {
		in   anthropicIn
		size int
		want bool
	}{
		"title on the small model":     {anthropicIn{Model: "claude-haiku-5-5", MaxTokens: 512}, 2000, true},
		"short answer on a main model": {anthropicIn{Model: "claude-opus-5-5", MaxTokens: 1024}, 2000, true},
		"main model, long answer":      {anthropicIn{Model: "claude-opus-5-5", MaxTokens: 32000}, 2000, false},
		"main model, no limit":         {anthropicIn{Model: "claude-opus-5-5"}, 2000, false},
		"small model with tools":       {anthropicIn{Model: "claude-haiku-5-5", MaxTokens: 512, Tools: []json.RawMessage{tool}}, 2000, false},
		"small model, large body":      {anthropicIn{Model: "claude-haiku-5-5", MaxTokens: 512}, quickBody + 1, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, quick(tc.in, tc.size), tc.want)
		})
	}
}

// TestQuickRequestsQueueFirst checks that a side request waits for its slot
// with AcquireFirst and the main agent's with Acquire.
func TestQuickRequestsQueueFirst(t *testing.T) {
	loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, completeAnswer) }))
	defer loc.Close()
	cases := map[string]struct {
		body  string
		first bool
	}{
		"side request": {`{"model":"claude-haiku-5-5","max_tokens":512,"messages":[{"role":"user","content":"title"}]}`, true},
		"main agent": {`{"model":"claude-opus-5-5","max_tokens":32000,"tools":[{"name":"Bash","input_schema":{"type":"object"}}],` +
			`"messages":[{"role":"user","content":"fix it"}]}`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var used string
			l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen",
				Acquire:      func(context.Context) (func(), error) { used = "ordinary"; return func() {}, nil },
				AcquireFirst: func(context.Context) (func(), error) { used = "first"; return func() {}, nil },
			}
			rec := httptest.NewRecorder()
			l.serve(context.Background(), rec, []byte(tc.body))
			want := "ordinary"
			if tc.first {
				want = "first"
			}
			assert.Equal(t, used, want)
		})
	}
	t.Run("no first queue", func(t *testing.T) {
		var used bool
		l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen",
			Acquire: func(context.Context) (func(), error) { used = true; return func() {}, nil }}
		l.serve(context.Background(), httptest.NewRecorder(), []byte(cases["side request"].body))
		assert.True(t, used, "without AcquireFirst a quick request takes the ordinary queue")
	})
}
