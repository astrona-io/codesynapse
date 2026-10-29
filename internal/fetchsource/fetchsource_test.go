package fetchsource

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/netguard"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const page = `<!doctype html><html><head><title>Widgets API</title><style>body{color:red}</style>
<script>alert("x")</script></head><body><nav>Home</nav><h1>Widgets</h1><p>Create a   widget
with <code>New</code>.</p><ul><li>fast</li><li>small</li></ul><pre>w := New()
w.Run()</pre><p>key ghp_abcdefghijklmnopqrstuvwxyz0123456789AB</p></body></html>`

// testServer is an httptest TLS server whose requests are counted.
type testServer struct {
	*httptest.Server
	hits atomic.Int32
}

func newServer(t *testing.T, h http.HandlerFunc) *testServer {
	t.Helper()
	ts := &testServer{}
	ts.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.hits.Add(1)
		h(w, r)
	}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are expected
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func (ts *testServer) addr() string { return strings.TrimPrefix(ts.URL, "https://") }

// testFetcher trusts the test servers' certificate and lets only the
// listed addresses through the dial guard; everything else meets the real
// guard, so a loopback server that is not listed is refused like any other
// internal address.
func testFetcher(t *testing.T, cache *Cache, allowed ...*testServer) *Fetcher {
	t.Helper()
	roots := x509.NewCertPool()
	ok := map[string]bool{}
	for _, ts := range allowed {
		roots.AddCert(ts.Certificate())
		ok[ts.addr()] = true
	}
	guard := netguard.Control(ErrRefused)
	control := func(network, address string, c syscall.RawConn) error {
		if ok[address] {
			return nil
		}
		return guard(network, address, c)
	}
	return &Fetcher{client: newClient(control, roots), cache: cache, now: time.Now}
}

func openCache(t *testing.T) *Cache {
	t.Helper()
	c, err := OpenCache(filepath.Join(t.TempDir(), "codesynapse", "sources"), 0)
	require.NoError(t, err)
	return c
}

func TestFetchExtractsTextAndCachesIt(t *testing.T) {
	ts := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Header.Get("User-Agent"), UserAgent)
		assert.Equal(t, r.Header.Get("Authorization"), "")
		assert.Equal(t, r.Header.Get("Cookie"), "")
		assert.Equal(t, r.URL.RawQuery, "a=1&b=2", "tracking parameters are dropped and the query sorted")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", `"v1"`)
		http.SetCookie(w, &http.Cookie{Name: "track", Value: "me"})
		_, _ = io.WriteString(w, page)
	})
	f := testFetcher(t, openCache(t), ts)
	out, err := f.Fetch(context.Background(), ts.URL+"/widgets?b=2&utm_source=x&a=1#intro")
	require.NoError(t, err)
	assert.Equal(t, out.Cache, Fetched)
	assert.Equal(t, out.Notice, Notice)
	assert.Equal(t, out.Host, "127.0.0.1")
	assert.Equal(t, out.ContentType, "text/html")
	assert.Equal(t, out.RawBytes, len(page))
	assert.Equal(t, out.TextBytes, len(out.Text))
	assert.True(t, out.TextTokens > 0 && out.TextTokens < out.RawTokens)
	for _, want := range []string{"Widgets API", "# Widgets", "Create a widget with New.", "- fast", "- small", "```\nw := New()\nw.Run()\n```"} {
		assert.Contains(t, out.Text, want)
	}
	for _, gone := range []string{"alert", "color:red", "<p>", "ghp_abc"} {
		assert.NotContains(t, out.Text, gone)
	}
	assert.Contains(t, out.Text, "[REDACTED]", "page text passes redact.Free")

	again, err := f.Fetch(context.Background(), ts.URL+"/widgets?a=1&b=2")
	require.NoError(t, err)
	assert.Equal(t, again.Cache, Hit)
	assert.Equal(t, again.Text, out.Text)
	assert.Equal(t, ts.hits.Load(), int32(1), "a fresh entry is answered without a request")
}

func TestFetchRevalidatesAStaleEntry(t *testing.T) {
	version := "v1"
	ts := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		etag := `"` + version + `"`
		if r.Header.Get("If-None-Match") == etag && r.Header.Get("If-Modified-Since") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/markdown")
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", "Mon, 26 Oct 2026 10:00:00 GMT")
		_, _ = fmt.Fprintf(w, "# Changelog\n\nversion %s\n", version)
	})
	f := testFetcher(t, openCache(t), ts)
	clock := time.Now()
	f.now = func() time.Time { return clock }

	first, err := f.Fetch(context.Background(), ts.URL+"/CHANGELOG.md")
	require.NoError(t, err)
	assert.Equal(t, first.Cache, Fetched)

	clock = clock.Add(DefaultTTL + time.Minute)
	second, err := f.Fetch(context.Background(), ts.URL+"/CHANGELOG.md")
	require.NoError(t, err)
	assert.Equal(t, second.Cache, Revalidated)
	assert.Equal(t, second.Text, first.Text)
	assert.Equal(t, ts.hits.Load(), int32(2))

	// The 304 renewed the entry's age.
	third, err := f.Fetch(context.Background(), ts.URL+"/CHANGELOG.md")
	require.NoError(t, err)
	assert.Equal(t, third.Cache, Hit)

	version = "v2"
	clock = clock.Add(DefaultTTL + time.Minute)
	fourth, err := f.Fetch(context.Background(), ts.URL+"/CHANGELOG.md")
	require.NoError(t, err)
	assert.Equal(t, fourth.Cache, Fetched)
	assert.Contains(t, fourth.Text, "version v2")
}

func TestFetchRedirects(t *testing.T) {
	other := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "internal secret")
	})
	var ts *testServer
	ts = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/final":
			assert.Equal(t, r.Header.Get("If-None-Match"), "", "conditional headers never follow a redirect")
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "arrived")
		case strings.HasPrefix(p, "/hop/"):
			var n int
			_, _ = fmt.Sscanf(p, "/hop/%d", &n)
			next := "/final"
			if n > 1 {
				next = fmt.Sprintf("/hop/%d", n-1)
			}
			http.Redirect(w, r, next, http.StatusFound)
		case p == "/to-internal":
			http.Redirect(w, r, other.URL+"/", http.StatusMovedPermanently)
		case p == "/to-metadata":
			http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
		case p == "/to-http":
			http.Redirect(w, r, "http://"+ts.addr()+"/final", http.StatusFound)
		case p == "/to-credentials":
			http.Redirect(w, r, "https://user:pw@"+ts.addr()+"/final", http.StatusFound)
		}
	})
	f := testFetcher(t, nil, ts)

	out, err := f.Fetch(context.Background(), ts.URL+"/hop/5")
	require.NoError(t, err, "five redirects are followed")
	assert.Equal(t, out.Text, "arrived")

	_, err = f.Fetch(context.Background(), ts.URL+"/hop/6")
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Contains(t, err.Error(), "more than 5 redirects")

	for _, path := range []string{"/to-internal", "/to-metadata"} {
		_, err = f.Fetch(context.Background(), ts.URL+path)
		assert.ErrorIs(t, err, ErrRefused, path)
	}
	assert.Equal(t, other.hits.Load(), int32(0), "the internal server was never reached")

	for _, path := range []string{"/to-http", "/to-credentials"} {
		_, err = f.Fetch(context.Background(), ts.URL+path)
		assert.ErrorIs(t, err, ErrInvalid, path)
	}
}

func TestFetchRefusesInternalAddresses(t *testing.T) {
	ts := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "local")
	})
	_, port, _ := net.SplitHostPort(ts.addr())
	f := New(nil) // the real guard, nothing allowed
	for _, raw := range []string{
		ts.URL + "/",
		"https://localhost:" + port + "/",
		"https://[::1]:" + port + "/",
		"https://10.0.0.1/",
		"https://192.168.1.10/",
		"https://169.254.169.254/latest/meta-data/",
		"https://100.100.100.200/",
		"https://0.0.0.0:" + port + "/",
	} {
		_, err := f.Fetch(context.Background(), raw)
		assert.ErrorIs(t, err, ErrRefused, raw)
	}
	assert.Equal(t, ts.hits.Load(), int32(0))
}

func TestFetchRefusesInvalidURLs(t *testing.T) {
	f := New(nil)
	for _, raw := range []string{"", "http://example.com/", "ftp://example.com/", "https://user:pw@example.com/", "https:///path", "example.com", "mailto:a@example.com", "https://example.com/" + strings.Repeat("a", MaxURL)} {
		_, err := f.Fetch(context.Background(), raw)
		assert.ErrorIs(t, err, ErrInvalid, raw)
	}
}

func TestFetchLimits(t *testing.T) {
	ts := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(strings.Repeat("a", MaxBody+10)))
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG"))
		case "/binary":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("text"))
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"widget"}`)
		}
	})
	f := testFetcher(t, nil, ts)
	_, err := f.Fetch(context.Background(), ts.URL+"/big")
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Contains(t, err.Error(), "larger than")
	for _, path := range []string{"/image", "/binary"} {
		_, err = f.Fetch(context.Background(), ts.URL+path)
		assert.ErrorIs(t, err, ErrUnavailable, path)
		assert.Contains(t, err.Error(), "content type")
	}
	_, err = f.Fetch(context.Background(), ts.URL+"/missing")
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Contains(t, err.Error(), "404")
	out, err := f.Fetch(context.Background(), ts.URL+"/json")
	require.NoError(t, err)
	assert.Equal(t, out.Text, `{"name":"widget"}`)
}

func TestAnswerIsCappedOnARuneBoundary(t *testing.T) {
	u, err := Normalise("https://example.com/")
	require.NoError(t, err)
	text := strings.Repeat("é", MaxAnswer) // two bytes each
	out := answer(u, Entry{Text: text, RawBytes: len(text)}, Fetched)
	assert.True(t, out.Truncated)
	assert.LessOrEqual(t, len(out.Text), MaxAnswer)
	assert.Equal(t, strings.ToValidUTF8(out.Text, "?"), out.Text)
	assert.Equal(t, out.TextBytes, len(out.Text))
}

func TestRunRefusesUnknownArguments(t *testing.T) {
	_, err := New(nil).Run(context.Background(), []byte(`{"url":"https://example.com/","headers":{"Authorization":"x"}}`))
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestNormalise(t *testing.T) {
	a, err := Normalise("HTTPS://Docs.Example.COM:443/a/b?z=1&utm_medium=x&fbclid=y&a=2#frag")
	require.NoError(t, err)
	assert.Equal(t, a.String(), "https://docs.example.com/a/b?a=2&z=1")
	b, err := Normalise("https://docs.example.com/a/b?a=2&z=1")
	require.NoError(t, err)
	assert.Equal(t, Key(a), Key(b))
	c, err := Normalise("https://docs.example.com")
	require.NoError(t, err)
	assert.Equal(t, c.String(), "https://docs.example.com/")
	assert.Equal(t, TTL("docs.example.com"), DocTTL)
	assert.Equal(t, TTL("pkg.go.dev"), DocTTL)
	assert.Equal(t, TTL("news.example.com"), DefaultTTL)
}

func TestCachePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sources")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	c, err := OpenCache(dir, 0)
	require.NoError(t, err)
	require.NoError(t, c.Put("k", Entry{URL: "https://example.com/", Text: "hi"}))
	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o700), "an existing folder is tightened")
	fi, err = os.Stat(filepath.Join(dir, "k.json"))
	require.NoError(t, err)
	assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o600))

	fresh, err := OpenCache(filepath.Join(t.TempDir(), "a", "b"), 0)
	require.NoError(t, err)
	fi, err = os.Stat(fresh.dir)
	require.NoError(t, err)
	assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o700))
}

func TestCacheGetChecksTheEntry(t *testing.T) {
	c := openCache(t)
	require.NoError(t, c.Put("k", Entry{URL: "https://example.com/", Text: "hi"}))
	_, ok := c.Get("k", "https://example.com/other")
	assert.False(t, ok, "an entry for another URL is not used")
	e, ok := c.Get("k", "https://example.com/")
	assert.True(t, ok)
	assert.Equal(t, e.Text, "hi")

	target := filepath.Join(t.TempDir(), "elsewhere.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"url":"https://example.com/l","text":"planted"}`), 0o600))
	require.NoError(t, os.Symlink(target, c.path("link")))
	_, ok = c.Get("link", "https://example.com/l")
	assert.False(t, ok, "a symlink in the cache folder is not followed")

	var nilCache *Cache
	_, ok = nilCache.Get("k", "https://example.com/")
	assert.False(t, ok)
	assert.NoError(t, nilCache.Put("k", Entry{}))
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := openCache(t)
	text := strings.Repeat("x", 1000)
	put := func(key string, age time.Duration) {
		require.NoError(t, c.Put(key, Entry{URL: "https://example.com/" + key, Text: text}))
		when := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(c.path(key), when, when))
	}
	put("a", 3*time.Hour)
	put("b", 2*time.Hour)
	put("c", time.Hour)
	fi, err := os.Stat(c.path("a"))
	require.NoError(t, err)
	c.max = 3 * fi.Size()
	c.Touch("a") // used just now: b is the least recently used
	require.NoError(t, c.Put("d", Entry{URL: "https://example.com/d", Text: text}))

	exists := func(key string) bool { _, err := os.Stat(c.path(key)); return err == nil }
	assert.True(t, exists("a"))
	assert.False(t, exists("b"), "the least recently used entry went first")
	assert.True(t, exists("c"))
	assert.True(t, exists("d"))
}

func TestDefaultDir(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Skip("no user cache folder here")
	}
	assert.True(t, strings.HasSuffix(dir, filepath.Join("codesynapse", "sources")))
}

// An error from the dial guard keeps its identity through net/http's
// wrapping, which is what Fetch relies on.
func TestGuardErrorSurvivesWrapping(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://x/", Err: &net.OpError{Op: "dial", Err: ErrRefused}}
	assert.True(t, errors.Is(err, ErrRefused))
}
