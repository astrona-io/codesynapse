package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The bridge names the checkout's repository on calls that name none, only for
// tools that declare a repo argument, and never over an explicit one.
func TestBridgeFillsInTheCheckoutsRepository(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://github.com/acme/widgets.git"}} {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	t.Chdir(dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &m))
		w.Header().Set("Content-Type", "application/json")
		switch m.Method {
		case "initialize":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`, m.ID)
		case "tools/list":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"search","inputSchema":{"properties":{"repo":{}}}},{"name":"repo_list","inputSchema":{"properties":{}}}]}}`, m.ID)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n"
	b := &bridge{url: srv.URL, token: "csk_test", out: &bytes.Buffer{}, http: srv.Client()}
	require.NoError(t, b.run(context.Background(), strings.NewReader(in)))

	for _, tc := range []struct {
		name, msg, want string
	}{
		{"no arguments", `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"search"}}`, "acme/widgets"},
		{"other arguments kept", `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"search","arguments":{"query":"x"}}}`, "acme/widgets"},
		{"empty repo", `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"search","arguments":{"repo":""}}}`, "acme/widgets"},
		{"explicit repo wins", `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"search","arguments":{"repo":"other/repo"}}}`, "other/repo"},
		{"tool without repo", `{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"repo_list"}}`, "<none>"},
		{"unknown tool", `{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"nope"}}`, "<none>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m rpcMessage
			require.NoError(t, json.Unmarshal([]byte(tc.msg), &m))
			out := b.withRepo(context.Background(), []byte(tc.msg), m)
			var c struct {
				Params struct {
					Arguments struct {
						Repo  *string `json:"repo"`
						Query string  `json:"query"`
					} `json:"arguments"`
				} `json:"params"`
			}
			require.NoError(t, json.Unmarshal(out, &c))
			got := "<none>"
			if c.Params.Arguments.Repo != nil {
				got = *c.Params.Arguments.Repo
			}
			assert.Equal(t, got, tc.want)
			if tc.name == "other arguments kept" {
				assert.Equal(t, c.Params.Arguments.Query, "x")
			}
		})
	}
}
