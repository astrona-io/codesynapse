package gateway

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const usageStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":12,"cache_read_input_tokens":9000,"cache_creation_input_tokens":300,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"\"usage\": not this one"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n"

func TestCloudUsageIsRead(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		chunk                   int
		want                    Meta
	}{
		{"stream whole", "text/event-stream", usageStream, 0,
			Meta{CloudIn: 12, CloudCached: 9000, CloudCacheWrite: 300, CloudOut: 42, CloudMetered: true}},
		{"stream in small pieces", "text/event-stream; charset=utf-8", usageStream, 7,
			Meta{CloudIn: 12, CloudCached: 9000, CloudCacheWrite: 300, CloudOut: 42, CloudMetered: true}},
		{"stream with CRLF and no final newline", "text/event-stream",
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\r\n" +
				`data: {"type":"message_delta","usage":{"output_tokens":3}}`, 0,
			Meta{CloudIn: 5, CloudOut: 3, CloudMetered: true}},
		{"json answer", "application/json",
			`{"id":"m","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":7,"cache_read_input_tokens":100,"output_tokens":2}}`, 0,
			Meta{CloudIn: 7, CloudCached: 100, CloudOut: 2, CloudMetered: true}},
		{"no usage", "text/event-stream", "event: ping\n\n", 0, Meta{}},
		{"bad json", "application/json", `{"usage":`, 0, Meta{}},
		{"other type", "text/plain", `{"usage":{"input_tokens":1}}`, 0, Meta{}},
		{"line too long is skipped", "text/event-stream",
			`data: {"usage":{"input_tokens":1},"pad":"` + strings.Repeat("x", maxUsageLine) + "\"}\n" +
				`data: {"usage":{"output_tokens":2}}` + "\n", 0,
			Meta{CloudOut: 2, CloudMetered: true}},
		{"json too large is skipped", "application/json",
			`{"usage":{"input_tokens":1},"pad":"` + strings.Repeat("x", maxUsageBody) + `"}`, 4096, Meta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCloudUsage(tc.contentType)
			b := []byte(tc.body)
			step := cmp.Or(tc.chunk, len(b))
			for len(b) > 0 {
				n := min(step, len(b))
				c.Write(b[:n])
				b = b[n:]
			}
			c.finish()
			var m Meta
			c.note(&m)
			assert.Equal(t, m, tc.want)
		})
	}
}

func TestForwardNotesCloudUsageAndMessages(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, usageStream)
	}))
	defer up.Close()
	var meta Meta
	g := NewAnthropic()
	g.Upstream = up.URL
	g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
	srv := httptest.NewServer(g.Handler("/v1/messages"))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`))
	req.Header.Set("X-Api-Key", "sk-ant-x")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assert.Equal(t, string(out), usageStream, "the answer passes unchanged")
	assert.Equal(t, meta.Messages, 3)
	assert.Equal(t, [5]any{meta.CloudIn, meta.CloudCached, meta.CloudCacheWrite, meta.CloudOut, meta.CloudMetered}, [5]any{12, 9000, 300, 42, true})
}

func TestInspectCountsMessagesOnlyForMessages(t *testing.T) {
	body := []byte(`{"messages":[{},{}]}`)
	assert.Equal(t, inspect("/v1/messages", body).Messages, 2)
	assert.Equal(t, inspect(CountPath, body).Messages, 0)
}

func FuzzCloudUsage(f *testing.F) {
	f.Add("text/event-stream", usageStream, 5)
	f.Add("application/json", `{"usage":{"input_tokens":7}}`, 3)
	f.Fuzz(func(t *testing.T, contentType, body string, chunk int) {
		c := newCloudUsage(contentType)
		b := []byte(body)
		step := max(1, chunk%64)
		for len(b) > 0 {
			n := min(step, len(b))
			c.Write(b[:n])
			b = b[n:]
			if c != nil && len(c.buf) > maxUsageBody {
				t.Fatal("buffer over its bound")
			}
		}
		c.finish()
		var m Meta
		c.note(&m)
		if m.CloudIn < 0 || m.CloudCached < 0 || m.CloudCacheWrite < 0 || m.CloudOut < 0 {
			t.Fatalf("negative usage %+v", m)
		}
	})
}
