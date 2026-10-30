package gatewayproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const testKey = "csk_test_key_0123456789"

// upstream is a fake gateway or Anthropic that records what reached it.
type upstream struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
	paths   []string
	bodies  []string
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.headers = append(u.headers, r.Header.Clone())
		u.paths = append(u.paths, r.URL.Path)
		u.bodies = append(u.bodies, string(b))
		u.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b))
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) calls(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, p := range u.paths {
		if p == path {
			n++
		}
	}
	return n
}

func (u *upstream) last() (http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.headers) == 0 {
		return nil, ""
	}
	return u.headers[len(u.headers)-1], u.bodies[len(u.bodies)-1]
}

// deadURL is an address nothing listens on.
func deadURL(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.NotFoundHandler())
	u := s.URL
	s.Close()
	return u
}

func newTestProxy(t *testing.T, gateway, direct string) (*Proxy, *httptest.Server) {
	t.Helper()
	files := Files{Dir: t.TempDir()}
	cfg := Config{Gateway: gateway, Port: DefaultPort}
	require.NoError(t, files.Save(cfg))
	secret, err := files.Secret()
	require.NoError(t, err)
	p, err := New(Options{Config: cfg, Files: files, Secret: secret, Cooldown: 20 * time.Millisecond})
	require.NoError(t, err)
	p.direct = direct
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv
}

// reply is what send got back; the body is already read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

func send(t *testing.T, base, auth string) (reply, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages?beta=true", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-CodeSynapse-Key", testKey)
	req.Header.Set("X-CodeSynapse-Other", "x")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return reply{resp.StatusCode, resp.Header}, string(b)
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"type":"message"}`)
}

func TestForwarding(t *testing.T) {
	cases := []struct {
		name       string
		gateway    http.HandlerFunc // nil: nothing listens
		wantStatus int
		wantDirect bool
	}{
		{"gateway up", ok, http.StatusOK, false},
		{"gateway down", nil, http.StatusOK, true},
		{"front proxy 502 html", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html>bad gateway</html>")
		}, http.StatusOK, true},
		{"front proxy 504 no type", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGatewayTimeout)
		}, http.StatusOK, true},
		{"gateway 401", jsonStatus(http.StatusUnauthorized), http.StatusUnauthorized, false},
		{"gateway 429", jsonStatus(http.StatusTooManyRequests), http.StatusTooManyRequests, false},
		{"gateway 403", jsonStatus(http.StatusForbidden), http.StatusForbidden, false},
		{"gateway json 503", jsonStatus(http.StatusServiceUnavailable), http.StatusServiceUnavailable, false},
		{"gateway json 502", jsonStatus(http.StatusBadGateway), http.StatusBadGateway, false},
		// The gateway relays Anthropic's status and type as they are; an
		// HTML 503 from Anthropic's edge came after the request went
		// upstream, so it must not be sent again.
		{"gateway relays html 503", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(RouteHeader, "cloud")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "<html>overloaded</html>")
		}, http.StatusServiceUnavailable, false},
		{"gateway's own html 502", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(RouteHeader, "gateway")
			w.WriteHeader(http.StatusBadGateway)
		}, http.StatusBadGateway, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			direct := newUpstream(t, ok)
			gwURL := deadURL(t)
			var gw *upstream
			if tc.gateway != nil {
				gw = newUpstream(t, tc.gateway)
				gwURL = gw.URL
			}
			p, srv := newTestProxy(t, gwURL, direct.URL)
			resp, _ := send(t, srv.URL, "Bearer sk-ant-own")
			assert.Equal(t, resp.StatusCode, tc.wantStatus)
			assert.Equal(t, p.Down(), tc.wantDirect)
			if !tc.wantDirect {
				assert.Equal(t, direct.calls("/v1/messages"), 0, "no failover")
				h, _ := gw.last()
				assert.Equal(t, h.Get("X-CodeSynapse-Key"), testKey, "key kept on the gateway route")
				assert.NotEqual(t, resp.Header.Get(RouteHeader), "direct")
				assert.Len(t, p.counts.pending(time.Now()), 0)
				return
			}
			assert.Equal(t, direct.calls("/v1/messages"), 1)
			h, body := direct.last()
			assert.Equal(t, h.Get("X-CodeSynapse-Key"), "", "the key never goes direct")
			assert.Equal(t, h.Get("X-CodeSynapse-Other"), "")
			assert.Equal(t, h.Get("Authorization"), "Bearer sk-ant-own")
			assert.Equal(t, h.Get("Anthropic-Version"), "2023-06-01")
			assert.Contains(t, body, "claude-sonnet-4-5", "the body is replayed")
			assert.Equal(t, resp.Header.Get(RouteHeader), "direct")
			p.counts.mu.Lock()
			defer p.counts.mu.Unlock()
			require.Len(t, p.counts.w, 1)
			assert.Equal(t, p.counts.w[0].Requests, 1)
			assert.Equal(t, p.counts.w[0].Models["claude-sonnet-4-5"], 1)
		})
	}
}

func jsonStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"type":"error"}`)
	}
}

func TestDirectRefusesCodeSynapseCredential(t *testing.T) {
	direct := newUpstream(t, ok)
	_, srv := newTestProxy(t, deadURL(t), direct.URL)
	resp, body := send(t, srv.URL, "Bearer "+testKey)
	assert.Equal(t, resp.StatusCode, http.StatusBadGateway)
	assert.Contains(t, body, "CodeSynapse key")
	assert.Equal(t, direct.calls("/v1/messages"), 0)
}

func TestMidStreamFailureIsNotRetried(t *testing.T) {
	direct := newUpstream(t, ok)
	gw := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	p, srv := newTestProxy(t, gw.URL, direct.URL)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusOK)
	assert.Contains(t, string(b), "message_start", "what streamed reached the client")
	assert.Equal(t, direct.calls("/v1/messages"), 0)
	assert.False(t, p.Down())
}

func TestRecoveryReportsCounts(t *testing.T) {
	var gatewayDown atomic.Bool
	gatewayDown.Store(true)
	var report atomic.Value
	direct := newUpstream(t, ok)
	gw := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if gatewayDown.Load() {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == ReportPath {
			if r.Header.Get("X-CodeSynapse-Key") != testKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			b, _ := io.ReadAll(r.Body)
			report.Store(string(b))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ok(w, r)
	})
	p, srv := newTestProxy(t, gw.URL, direct.URL)

	for range 2 {
		resp, _ := send(t, srv.URL, "Bearer sk-ant-own")
		assert.Equal(t, resp.Header.Get(RouteHeader), "direct")
	}
	assert.True(t, p.Down())
	assert.Equal(t, gw.calls("/v1/messages"), 1, "the breaker holds while down")

	gatewayDown.Store(false)
	waitFor(t, func() bool { return !p.Down() })
	waitFor(t, func() bool { return report.Load() != nil })
	var rep Report
	require.NoError(t, json.Unmarshal([]byte(report.Load().(string)), &rep))
	require.Len(t, rep.Windows, 1)
	assert.Equal(t, rep.Windows[0].Requests, 2)
	assert.Equal(t, rep.Windows[0].Models["claude-sonnet-4-5"], 2)
	assert.False(t, rep.Windows[0].End.Before(rep.Windows[0].Start))
	waitFor(t, func() bool { return len(p.counts.pending(time.Now())) == 0 })

	resp, _ := send(t, srv.URL, "Bearer sk-ant-own")
	assert.Equal(t, resp.StatusCode, http.StatusOK)
	assert.Equal(t, resp.Header.Get(RouteHeader), "", "back on the gateway")
	assert.Equal(t, direct.calls("/v1/messages"), 2)
}

func TestProbeTreatsGatewayRefusalAsUp(t *testing.T) {
	// A 401 on the probe is the gateway answering: its refusal must reach
	// Claude Code, not be avoided by going direct.
	var gatewayDown atomic.Bool
	gatewayDown.Store(true)
	gw := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		if gatewayDown.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		jsonStatus(http.StatusUnauthorized)(w, nil)
	})
	p, srv := newTestProxy(t, gw.URL, newUpstream(t, ok).URL)
	_, _ = send(t, srv.URL, "Bearer sk-ant-own")
	assert.True(t, p.Down())
	gatewayDown.Store(false)
	waitFor(t, func() bool { return !p.Down() })
	resp, _ := send(t, srv.URL, "Bearer sk-ant-own")
	assert.Equal(t, resp.StatusCode, http.StatusUnauthorized)
}

func TestRefusesBrowsers(t *testing.T) {
	gw := newUpstream(t, ok)
	_, srv := newTestProxy(t, gw.URL, newUpstream(t, ok).URL)
	cases := []struct {
		name string
		set  func(*http.Request)
	}{
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		{"fetch metadata", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }},
		{"rebound host", func(r *http.Request) { r.Host = "evil.example:" + strconv.Itoa(DefaultPort) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
			require.NoError(t, err)
			tc.set(req)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, resp.StatusCode, http.StatusForbidden)

			// Nor may a page read the health proof.
			req, err = http.NewRequest(http.MethodGet, srv.URL+HealthPath+"?nonce="+strings.Repeat("a", 32), nil)
			require.NoError(t, err)
			tc.set(req)
			resp, err = http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, resp.StatusCode, http.StatusForbidden)
		})
	}
	assert.Equal(t, gw.calls("/v1/messages"), 0)
}

func TestKeyIsPinned(t *testing.T) {
	gw := newUpstream(t, ok)
	other := func(t *testing.T, srv string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv+"/v1/messages", strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set(keyHeader, "csk_someone_else_0123456789")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("install digest", func(t *testing.T) {
		p, srv := newTestProxy(t, gw.URL, deadURL(t))
		p.cfg.KeyHash = HashKey(testKey)
		assert.Equal(t, other(t, srv.URL), http.StatusForbidden, "a key install did not write is refused")
		resp, _ := send(t, srv.URL, "Bearer sk-ant-own")
		assert.Equal(t, resp.StatusCode, http.StatusOK)
		assert.Equal(t, p.key, testKey)
	})
	t.Run("first key", func(t *testing.T) {
		p, srv := newTestProxy(t, gw.URL, deadURL(t))
		resp, _ := send(t, srv.URL, "Bearer sk-ant-own")
		assert.Equal(t, resp.StatusCode, http.StatusOK)
		assert.Equal(t, other(t, srv.URL), http.StatusForbidden)
		assert.Equal(t, p.key, testKey, "the first key stays")
	})
	assert.Equal(t, gw.calls("/v1/messages"), 2, "refused requests never reach the gateway")
}

// hangingGateway accepts connections and never reads or answers.
func hangingGateway(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "http://" + ln.Addr().String()
}

func TestHangingGateway(t *testing.T) {
	t.Run("took the request: fails, not resent", func(t *testing.T) {
		direct := newUpstream(t, ok)
		p, srv := newTestProxy(t, hangingGateway(t), direct.URL)
		p.gw = newClient(time.Second, 100*time.Millisecond)
		resp, _ := send(t, srv.URL, "Bearer sk-ant-own")
		assert.Equal(t, resp.StatusCode, http.StatusGatewayTimeout)
		assert.Equal(t, direct.calls("/v1/messages"), 0, "it may have been acted on")
		assert.True(t, p.Down(), "later requests go direct")
		resp, _ = send(t, srv.URL, "Bearer sk-ant-own")
		assert.Equal(t, resp.StatusCode, http.StatusOK)
		assert.Equal(t, direct.calls("/v1/messages"), 1)
	})
	t.Run("never took it in: goes direct", func(t *testing.T) {
		direct := newUpstream(t, ok)
		p, srv := newTestProxy(t, hangingGateway(t), direct.URL)
		p.gw = newClient(time.Second, time.Minute)
		p.writeWait = 200 * time.Millisecond
		// Larger than the socket buffers, so the write blocks.
		big := `{"model":"m","pad":"` + strings.Repeat("x", 24<<20) + `"}`
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(big))
		require.NoError(t, err)
		req.Header.Set(keyHeader, testKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusOK)
		assert.Equal(t, resp.Header.Get(RouteHeader), "direct")
		assert.Equal(t, direct.calls("/v1/messages"), 1)
		assert.True(t, p.Down())
	})
}

func TestValidateListen(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:18431", true},
		{"[::1]:18431", true},
		{"0.0.0.0:18431", false},
		{":18431", false},
		{"192.168.1.5:18431", false},
		{"localhost:18431", false}, // a name could resolve elsewhere
		{"example.com:18431", false},
		{"127.0.0.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			err := ValidateListen(tc.addr)
			assert.Equal(t, err == nil, tc.ok, err)
			if !tc.ok {
				_, lerr := Listen(context.Background(), tc.addr)
				assert.Error(t, lerr)
			}
		})
	}
}

func TestCheckPort(t *testing.T) {
	files := Files{Dir: t.TempDir()}
	secret, err := files.Secret()
	require.NoError(t, err)
	again, err := files.Secret()
	require.NoError(t, err)
	assert.Equal(t, string(again), string(secret), "the secret is kept")

	t.Run("free", func(t *testing.T) {
		assert.Equal(t, Check(context.Background(), portOf(t, deadURL(t)), secret), PortFree)
	})
	t.Run("stranger", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello")
		}))
		defer s.Close()
		assert.Equal(t, Check(context.Background(), portOf(t, s.URL), secret), PortStranger)
	})
	t.Run("ours with another secret", func(t *testing.T) {
		p, srv := newTestProxy(t, deadURL(t), deadURL(t))
		_ = p
		assert.Equal(t, Check(context.Background(), portOf(t, srv.URL), secret), PortStranger)
	})
	t.Run("ours", func(t *testing.T) {
		p, srv := newTestProxy(t, deadURL(t), deadURL(t))
		assert.Equal(t, Check(context.Background(), portOf(t, srv.URL), p.secret), PortOurs)
	})
}

func portOf(t *testing.T, raw string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(raw, "http://"))
	require.NoError(t, err)
	n, err := strconv.Atoi(port)
	require.NoError(t, err)
	return n
}

func TestRunStops(t *testing.T) {
	cases := []struct {
		name   string
		idle   time.Duration
		remove bool
	}{
		{"idle", 30 * time.Millisecond, false},
		{"config removed", time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := Files{Dir: t.TempDir()}
			cfg := Config{Gateway: deadURL(t), Port: DefaultPort}
			require.NoError(t, files.Save(cfg))
			secret, err := files.Secret()
			require.NoError(t, err)
			p, err := New(Options{Config: cfg, Files: files, Secret: secret, Idle: tc.idle})
			require.NoError(t, err)
			p.tick = 10 * time.Millisecond
			ln, err := Listen(context.Background(), "127.0.0.1:0")
			require.NoError(t, err)
			if tc.remove {
				_, err := files.Remove()
				require.NoError(t, err)
			}
			done := make(chan error, 1)
			go func() { done <- p.Run(context.Background(), ln) }()
			select {
			case err := <-done:
				assert.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the proxy did not stop")
			}
		})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
