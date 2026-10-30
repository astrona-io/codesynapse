package fetchsource

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestParseTTLs(t *testing.T) {
	t.Run("rules", func(t *testing.T) {
		ttls, err := ParseTTLs([]byte(`{"hosts": {
			"wiki.example.com": "10m",
			"*.example.com": "48h",
			"*.api.example.com": "2h",
			"pkg.go.dev": "1h",
			"127.0.0.1": "3h"
		}}`))
		require.NoError(t, err)
		assert.Equal(t, ttls.Len(), 5)
		for host, want := range map[string]time.Duration{
			"wiki.example.com":       10 * time.Minute, // exact beats a wildcard
			"Wiki.Example.COM":       10 * time.Minute,
			"blog.example.com":       48 * time.Hour,
			"v1.api.example.com":     2 * time.Hour, // the longest wildcard wins
			"example.com":            DefaultTTL,    // a wildcard is subdomains only
			"badexample.com":         DefaultTTL,
			"pkg.go.dev":             time.Hour, // overrides the built-in list
			"docs.rs":                DocTTL,    // the built-in list still applies
			"news.example.org":       DefaultTTL,
			"127.0.0.1":              3 * time.Hour,
			"docs.unknown.invalid":   DocTTL,
			"x.docs.unknown.invalid": DefaultTTL,
		} {
			assert.Equal(t, ttls.For(host), want, host)
		}
	})

	t.Run("nil uses the built-in list", func(t *testing.T) {
		var ttls *TTLs
		assert.Equal(t, ttls.For("pkg.go.dev"), DocTTL)
		assert.Equal(t, ttls.For("news.example.com"), DefaultTTL)
		assert.Equal(t, ttls.Len(), 0)
	})

	var b strings.Builder
	b.WriteString(`{"hosts":{`)
	for i := range MaxTTLHosts + 1 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"h%d.example.com":"1h"`, i)
	}
	b.WriteString(`}}`)

	for name, in := range map[string]string{
		"not json":           `hosts: {}`,
		"unknown field":      `{"hosts": {}, "default": "1h"}`,
		"trailing data":      `{"hosts": {}} {}`,
		"ttl too short":      `{"hosts": {"a.example.com": "30s"}}`,
		"ttl too long":       `{"hosts": {"a.example.com": "721h"}}`,
		"ttl negative":       `{"hosts": {"a.example.com": "-1h"}}`,
		"ttl not a duration": `{"hosts": {"a.example.com": "a week"}}`,
		"ttl a number":       `{"hosts": {"a.example.com": 3600}}`,
		"upper case":         `{"hosts": {"Docs.example.com": "1h"}}`,
		"port":               `{"hosts": {"docs.example.com:443": "1h"}}`,
		"scheme":             `{"hosts": {"https://docs.example.com": "1h"}}`,
		"path":               `{"hosts": {"docs.example.com/a": "1h"}}`,
		"bare star":          `{"hosts": {"*": "1h"}}`,
		"star tld":           `{"hosts": {"*.com": "1h"}}`,
		"inner star":         `{"hosts": {"docs.*.com": "1h"}}`,
		"empty label":        `{"hosts": {"docs..com": "1h"}}`,
		"hyphen edge":        `{"hosts": {"-docs.example.com": "1h"}}`,
		"empty host":         `{"hosts": {"": "1h"}}`,
		"long label":         `{"hosts": {"` + strings.Repeat("a", 64) + `.com": "1h"}}`,
		"too many":           b.String(),
		"too large":          `{"hosts": {}}` + strings.Repeat(" ", MaxTTLFile),
	} {
		t.Run(name, func(t *testing.T) {
			ttls, err := ParseTTLs([]byte(in))
			require.Error(t, err)
			assert.True(t, ttls == nil)
			assert.False(t, strings.Contains(err.Error(), "example.com"), "errors never name a host")
		})
	}
}

func TestLoadTTLs(t *testing.T) {
	dir := t.TempDir()

	ttls, err := LoadTTLs(filepath.Join(dir, "missing.json"))
	require.NoError(t, err)
	assert.True(t, ttls == nil)

	good := filepath.Join(dir, TTLFileName)
	require.NoError(t, os.WriteFile(good, []byte(`{"hosts":{"*.example.com":"2h"}}`), 0o600))
	ttls, err = LoadTTLs(good)
	require.NoError(t, err)
	assert.Equal(t, ttls.For("a.example.com"), 2*time.Hour)

	link := filepath.Join(dir, "link.json")
	require.NoError(t, os.Symlink(good, link))
	_, err = LoadTTLs(link)
	require.Error(t, err)

	_, err = LoadTTLs(dir)
	require.Error(t, err)

	big := filepath.Join(dir, "big.json")
	require.NoError(t, os.WriteFile(big, []byte(`{"hosts":{}}`+strings.Repeat(" ", MaxTTLFile)), 0o600))
	_, err = LoadTTLs(big)
	require.Error(t, err)
}

func TestDefaultTTLFile(t *testing.T) {
	p, err := DefaultTTLFile()
	if err != nil {
		t.Skip("no user config folder")
	}
	assert.Equal(t, filepath.Base(p), TTLFileName)
	assert.Equal(t, filepath.Base(filepath.Dir(p)), "codesynapse")
}

// TestFetchUsesConfiguredTTLs checks that a host rule decides freshness, also
// for a page cached under an older rule.
func TestFetchUsesConfiguredTTLs(t *testing.T) {
	ts := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello"))
	})
	f := testFetcher(t, openCache(t), ts)
	clock := time.Now()
	f.now = func() time.Time { return clock }

	_, err := f.Fetch(context.Background(), ts.URL+"/a")
	require.NoError(t, err)

	ttls, err := ParseTTLs([]byte(`{"hosts":{"127.0.0.1":"3h"}}`))
	require.NoError(t, err)
	f.WithTTLs(ttls)
	clock = clock.Add(2 * time.Hour) // past DefaultTTL, within the rule
	out, err := f.Fetch(context.Background(), ts.URL+"/a")
	require.NoError(t, err)
	assert.Equal(t, out.Cache, Hit)

	clock = clock.Add(2 * time.Hour)
	out, err = f.Fetch(context.Background(), ts.URL+"/a")
	require.NoError(t, err)
	assert.Equal(t, out.Cache, Fetched)
	assert.Equal(t, ts.hits.Load(), int32(2))
}

func FuzzParseTTLs(f *testing.F) {
	f.Add([]byte(`{"hosts":{"docs.example.com":"24h","*.example.org":"168h"}}`))
	f.Add([]byte(`{"hosts":{"*.":"1h"}}`))
	f.Add([]byte(`{"hosts":null}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, in []byte) {
		ttls, err := ParseTTLs(in)
		if err != nil {
			if ttls != nil {
				t.Fatal("rules returned with an error")
			}
			return
		}
		if ttls.Len() > MaxTTLHosts {
			t.Fatalf("%d rules", ttls.Len())
		}
		for h, d := range ttls.exact {
			if !validHost(h) || d < MinHostTTL || d > MaxHostTTL {
				t.Fatalf("accepted %q %s", h, d)
			}
			if ttls.For(h) != d {
				t.Fatalf("For(%q) = %s, want %s", h, ttls.For(h), d)
			}
		}
		for s, d := range ttls.suffix {
			if !strings.HasPrefix(s, ".") || !validHost(s[1:]) || d < MinHostTTL || d > MaxHostTTL {
				t.Fatalf("accepted wildcard %q %s", s, d)
			}
		}
	})
}
