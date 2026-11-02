package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// closedPort is an address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// silentListener accepts connections and never answers on them: a server
// that hangs, or a TLS endpoint that never finishes the handshake.
func silentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case <-done:
				_ = c.Close()
				return
			default:
				held = append(held, c)
			}
		}
	}()
	t.Cleanup(func() { close(done); _ = ln.Close() })
	return ln.Addr().String()
}

// forget removes the outage mark of base before and after a test, since the
// test cache is shared.
func forget(t *testing.T, base string) {
	t.Helper()
	clearOutage(base)
	t.Cleanup(func() { clearOutage(base) })
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestConnectFailure(t *testing.T) {
	_, refused := net.Dial("tcp", closedPort(t))
	require.Error(t, refused)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"caller gave up", fmt.Errorf("get: %w", context.Canceled), ""},
		{"request budget", context.DeadlineExceeded, ""},
		{"connection refused", fmt.Errorf("post: %w", refused), "refused"},
		{"dns", &net.DNSError{Err: "no such host", Name: "api.invalid", IsNotFound: true}, "dns"},
		{"dial timeout", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}, "dial-timeout"},
		{"dial other", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("network is unreachable")}, "dial"},
		{"reset after accept", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, ""},
		{"tls handshake timeout", errors.New("net/http: TLS handshake timeout"), "tls"},
		{"tls record", fmt.Errorf("get: %w", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}), "tls"},
		{"proxy 502", &statusError{Status: 502, msg: "bad gateway"}, "gateway"},
		{"proxy 504", &mcpStatusError{Status: 504}, "gateway"},
		{"refused key", &statusError{Status: 401, msg: "unauthorised"}, ""},
		{"forbidden", &statusError{Status: 403, msg: "forbidden"}, ""},
		{"server error", &statusError{Status: 500, msg: "internal error"}, ""},
		{"other", errors.New("decode: unexpected EOF"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, connectFailure(tc.err), tc.want)
		})
	}
}

func TestOutageMarkFile(t *testing.T) {
	base := "https://codesynapse.example.test:8443"
	forget(t, base)
	file := outageFile(base)
	require.True(t, file != "")
	assert.NotContains(t, file, "example")
	assert.NotContains(t, file, "8443")
	assert.Equal(t, outageFile(base+"/"), file, "a trailing slash is the same server")
	assert.True(t, outageFile("https://other.example.test") != file)
	assert.Equal(t, outageFile(""), "")

	assert.False(t, serverDown(base))
	markOutage(base, "made-up")
	assert.False(t, serverDown(base), "an unknown reason is not written")
	markOutage(base, "refused")
	assert.True(t, serverDown(base))
	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0o600))
	dir, err := os.Stat(filepath.Dir(file))
	require.NoError(t, err)
	assert.Equal(t, dir.Mode().Perm(), os.FileMode(0o700))
	data, err := os.ReadFile(file) //nolint:gosec // test file
	require.NoError(t, err)
	assert.NotContains(t, string(data), "example")
	m, ok := readOutage(base)
	require.True(t, ok)
	assert.Equal(t, m.Reason, "refused")

	clearOutage(base)
	assert.False(t, serverDown(base))

	// A damaged or oversized file is no mark.
	require.NoError(t, os.WriteFile(file, []byte("{not json"), 0o600))
	assert.False(t, serverDown(base))
	require.NoError(t, os.WriteFile(file, bytes.Repeat([]byte(" "), maxMarkBytes+1), 0o600))
	assert.False(t, serverDown(base))
}

func TestOutageMarkFresh(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"just now", now, true},
		{"a minute ago", now.Add(-time.Minute), true},
		{"past the window", now.Add(-outageFresh - time.Second), false},
		{"slightly ahead of the clock", now.Add(30 * time.Second), true},
		{"far in the future", now.Add(24 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, outageMark{At: tc.at, Reason: "dns"}.fresh(now), tc.want)
		})
	}
}

func FuzzOutageMark(f *testing.F) {
	f.Add([]byte(`{"at":"2026-11-02T12:00:00Z","reason":"refused"}`))
	f.Add([]byte(`{"at":"0001-01-01T00:00:00Z","reason":"dns"}`))
	f.Add([]byte(`{"at":"2026-11-02T12:00:00Z","reason":"https://host"}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, ok := parseOutageMark(data)
		if !ok {
			return
		}
		_, known := outageReasons[m.Reason]
		assert.True(t, known)
		assert.False(t, m.At.IsZero())
		assert.True(t, len(data) <= maxMarkBytes)
	})
}

func FuzzBriefNote(f *testing.F) {
	f.Add([]byte(`{"at":"2026-11-02T12:00:00Z","ms":3100,"outcome":"timeout"}`))
	f.Add([]byte(`{"at":"2026-11-02T12:00:00Z","ms":-1,"outcome":"briefed"}`))
	f.Add([]byte(`{"at":"2026-11-02T12:00:00Z","ms":10,"outcome":"other"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		n, ok := parseBriefNote(data)
		if !ok {
			return
		}
		_, known := briefOutcomes[n.Outcome]
		assert.True(t, known)
		assert.False(t, n.At.IsZero())
		assert.True(t, n.Millis >= 0)
	})
}

// TestHookClientUnreachable: the first hook that cannot connect waits one
// short budget and marks the server; every hook after it skips at once.
func TestHookClientUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		base   string
		reason string
		bound  time.Duration
	}{
		{"closed port", "http://" + closedPort(t), "refused", time.Second},
		{"handshake never answered", "https://" + silentListener(t), "tls", hookTLSTimeout + time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forget(t, tc.base)
			c := newHookClient(tc.base)
			started := time.Now()
			err := c.do(context.Background(), http.MethodGet, "/api/v1/agent-rules", nil, nil)
			took := time.Since(started)
			require.Error(t, err)
			assert.True(t, took < tc.bound, "first call took %s, want under %s", took, tc.bound)
			m, ok := readOutage(tc.base)
			require.True(t, ok, "the failure is marked")
			assert.Equal(t, m.Reason, tc.reason)

			started = time.Now()
			err = newHookClient(tc.base).do(context.Background(), http.MethodGet, "/api/v1/agent-rules", nil, nil)
			assert.ErrorIs(t, err, errServerOffline)
			assert.True(t, time.Since(started) < 100*time.Millisecond, "a marked server is skipped")
		})
	}
}

func TestHookClientMarkFollowsAnswers(t *testing.T) {
	status, contentType := http.StatusOK, "application/json"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	forget(t, srv.URL)
	c := newAPIClient(srv.URL)
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		down        bool
	}{
		{"proxy says bad gateway", http.StatusBadGateway, "text/html", true},
		{"answer clears it", http.StatusOK, "application/json", false},
		{"proxy gateway timeout", http.StatusGatewayTimeout, "text/plain", true},
		{"a refusal is the server answering", http.StatusUnauthorized, "application/problem+json", false},
		{"proxy unavailable", http.StatusServiceUnavailable, "", true},
		{"the API's own 503", http.StatusServiceUnavailable, "application/problem+json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, contentType = tc.status, tc.contentType
			_ = c.do(context.Background(), http.MethodGet, "/x", nil, nil)
			assert.Equal(t, serverDown(srv.URL), tc.down)
		})
	}
}

func TestBridgeSharesOutage(t *testing.T) {
	api := "http://127.0.0.1:18080"
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errors.New("connection refused"))}
	_, realRefused := net.Dial("tcp", closedPort(t))
	for _, tc := range []struct {
		name string
		mcp  string
		err  error
		down bool
	}{
		{"same origin refused", api + "/mcp", realRefused, true},
		{"other port refused", "http://127.0.0.1:18090/mcp", realRefused, false},
		{"other port, host gone", "http://127.0.0.1:18090/mcp", &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}, true},
		{"other host", "http://10.0.0.1:18080/mcp", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}, false},
		{"not a connect failure", api + "/mcp", &mcpStatusError{Status: 401}, false},
		{"dial error", api + "/mcp", refused, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forget(t, api)
			b := &bridge{url: tc.mcp, report: newAPIClient(api)}
			b.shareOutage(tc.err)
			assert.Equal(t, serverDown(api), tc.down)
			b.shareOnline()
			assert.Equal(t, serverDown(api), tc.down && !sameOrigin(tc.mcp, api))
		})
	}
}

func TestOutageMarkKeepsTheChatOffline(t *testing.T) {
	addr := closedPort(t)
	t.Setenv("CODESYNAPSE_API_URL", "http://"+addr)
	forget(t, "http://"+addr)
	assert.False(t, codesynapseOffline("outage-chat"))
	markOutage("http://"+addr, "refused")
	assert.True(t, codesynapseOffline("outage-chat"), "every chat skips a marked server")
}

// runBrief runs the pre-brief hook as Claude Code does and returns its
// output and how long it took.
func runBrief(t *testing.T, api, session, dir string) (string, time.Duration) {
	t.Helper()
	in, err := json.Marshal(hookInput{SessionID: session, Cwd: dir, Prompt: "make One return two instead of one"})
	require.NoError(t, err)
	var out bytes.Buffer
	o := &prebriefOptions{EC: &app.Env{Stdin: bytes.NewReader(in), Stdout: &out}, apiURL: api}
	started := time.Now()
	require.NoError(t, o.Run(context.Background(), nil), "the hook never fails")
	return out.String(), time.Since(started)
}

func TestPrebriefUnreachable(t *testing.T) {
	t.Setenv(apiTokenEnv, "csk_test")
	t.Setenv("CODESYNAPSE_PREBRIEF_SECONDS", "")
	dir := askCheckout(t)

	t.Run("closed port", func(t *testing.T) {
		api := "http://" + closedPort(t)
		forget(t, api)
		out, took := runBrief(t, api, "brief-closed-1", dir)
		assert.Equal(t, out, "")
		assert.True(t, took < 2*time.Second, "took %s", took)
		assert.True(t, serverDown(api))
		n, ok := lastBrief()
		require.True(t, ok)
		assert.Equal(t, n.Outcome, briefOffline)

		// Another chat skips the server without trying.
		out, took = runBrief(t, api, "brief-closed-2", dir)
		assert.Equal(t, out, "")
		assert.True(t, took < 200*time.Millisecond, "took %s", took)
	})

	t.Run("brief plan never answers", func(t *testing.T) {
		repo := filepath.Base(dir)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v1/execution/sessions":
				_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "org", SubjectID: "u", SessionID: "brief-slow-1", ParentSessionID: "brief-slow-1"})
			case r.URL.Path == "/api/v1/repos":
				_ = json.NewEncoder(w).Encode(map[string]any{"repos": []map[string]string{{"name": repo}}})
			case strings.HasSuffix(r.URL.Path, "/brief-plan"):
				// Read the body so the server notices the client hanging up.
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-time.After(20 * time.Second):
				}
			default:
				t.Errorf("unexpected %s", r.URL.Path)
			}
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(srv.CloseClientConnections)
		forget(t, srv.URL)
		out, took := runBrief(t, srv.URL, "brief-slow-1", dir)
		assert.Equal(t, out, "")
		assert.True(t, took >= briefPlanBudget && took < briefPlanBudget+2*time.Second, "took %s", took)
		assert.True(t, briefSkipped("brief-slow-1"))
		assert.False(t, serverDown(srv.URL), "a slow server is not an outage")
		n, ok := lastBrief()
		require.True(t, ok)
		assert.Equal(t, n.Outcome, briefTimeout)
		assert.Contains(t, briefCheck(time.Now()).Detail, "timed out")

		// The rest of the chat is not briefed; other chats still try.
		out, took = runBrief(t, srv.URL, "brief-slow-1", dir)
		assert.Equal(t, out, "")
		assert.True(t, took < 200*time.Millisecond, "took %s", took)
		n, _ = lastBrief()
		assert.Equal(t, n.Outcome, briefSkippedChat)
		assert.False(t, briefSkipped("brief-slow-2"))
	})
}

func TestBriefSkipFileRefusesBadSessions(t *testing.T) {
	for _, s := range []string{"", "../escape", "a/b", strings.Repeat("x", 500)} {
		assert.Equal(t, briefSkipFile(s), "", s)
		skipBriefs(s)
		assert.False(t, briefSkipped(s), s)
	}
}

func TestOutageCheck(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		mark     outageMark
		marked   bool
		serverOK bool
		want     string
	}{
		{"none", outageMark{}, false, true, "not set"},
		{"fresh and down", outageMark{At: now.Add(-40 * time.Second), Reason: "refused"}, true, false, "set 40s ago (connection refused); hooks skip"},
		{"fresh, answers now", outageMark{At: now.Add(-40 * time.Second), Reason: "dns"}, true, true, "cleared"},
		{"stale", outageMark{At: now.Add(-time.Hour), Reason: "tls"}, true, false, "stale, from 1h0m0s ago"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := outageCheck(tc.mark, tc.marked, tc.serverOK, now)
			assert.True(t, c.OK, "informational")
			assert.Contains(t, c.Detail, tc.want)
		})
	}
}
