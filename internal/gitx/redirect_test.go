package gitx

import (
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// trust makes git accept the test servers' certificates.
func trust(t *testing.T, servers ...*httptest.Server) {
	t.Helper()
	var pemData []byte
	for _, s := range servers {
		pemData = append(pemData, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})...)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, pemData, 0o600))
	testEnv = []string{"GIT_SSL_CAINFO=" + ca}
	t.Cleanup(func() { testEnv = nil })
}

// A redirect is not followed: the destination is never contacted, however
// the first address was validated, and the error says what to do.
func TestRemoteRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "should never be reached", http.StatusTeapot)
	}))
	t.Cleanup(target.Close)
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path+"?"+r.URL.RawQuery, http.StatusMovedPermanently)
	}))
	t.Cleanup(first.Close)
	trust(t, first, target)

	ctx := context.Background()
	remote := first.URL + "/acme/moved.git"
	for name, run := range map[string]func() error{
		"CanRead":        func() error { return CanRead(ctx, remote, Auth{}) },
		"RemoteBranches": func() error { _, err := RemoteBranches(ctx, remote, Auth{}); return err },
		"RemoteHead":     func() error { _, err := RemoteHead(ctx, remote, "main", Auth{}); return err },
		"Clone":          func() error { return Clone(ctx, remote, "", filepath.Join(t.TempDir(), "c"), Auth{}) },
	} {
		err := run()
		assert.True(t, errors.Is(err, ErrRedirected), name+": a redirect is reported as such: %v", err)
	}
	assert.Equal(t, int(hits.Load()), 0, "the redirect destination was never contacted")
}

// The environment git runs with carries the policy, so a change of it is a
// test failure rather than a silent loosening.
func TestRemoteEnvDisablesRedirects(t *testing.T) {
	env := strings.Join(remoteEnv("https://example.com/a/b.git", Auth{}), "\n")
	assert.Contains(t, env, "http.followRedirects")
	assert.Contains(t, env, "GIT_CONFIG_VALUE_2=false")
	assert.NotContains(t, env, "followRedirects=initial")
}

func TestValidatedAddressesArePinnedForGit(t *testing.T) {
	remote := "https://pin.example.com:8443/o/r.git"
	assert.Equal(t, pinFor(remote), "", "nothing validated yet")

	resolver := fakeResolver{"pin.example.com": {{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("2606:2800:220:1::1")}}}
	got, err := ValidateRemote(context.Background(), remote, RemoteOptions{Resolver: resolver})
	require.NoError(t, err)
	assert.Equal(t, got, remote)
	assert.Equal(t, pinFor(remote), "pin.example.com:8443:93.184.216.34,[2606:2800:220:1::1]")
	env := strings.Join(remoteEnv(remote, Auth{}), "\n")
	assert.Contains(t, env, "http.curloptResolve")
	assert.Contains(t, env, "pin.example.com:8443:93.184.216.34,[2606:2800:220:1::1]")
	assert.Equal(t, pinFor("https://pin.example.com/o/r.git"), "", "another port is another pin")

	// A rejected remote leaves no pin behind, and a literal IP needs none.
	_, err = ValidateRemote(context.Background(), "https://rebind.example.com/o/r.git", RemoteOptions{Resolver: fakeResolver{"rebind.example.com": {{IP: net.ParseIP("10.0.0.5")}}}})
	assert.True(t, errors.Is(err, ErrInvalidRemote))
	assert.Equal(t, pinFor("https://rebind.example.com/o/r.git"), "")
	u, _ := url.Parse("https://93.184.216.34/o/r.git")
	rememberPin(u, []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}})
	assert.Equal(t, pinFor(u.String()), "")
}

func TestPinsExpire(t *testing.T) {
	u, _ := url.Parse("https://expire.example.com/o/r.git")
	rememberPin(u, []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}})
	assert.True(t, pinFor(u.String()) != "")
	pins.Lock()
	p := pins.m[pinKey(u)]
	p.until = time.Now().Add(-time.Second)
	pins.m[pinKey(u)] = p
	pins.Unlock()
	assert.Equal(t, pinFor(u.String()), "")
}

// git itself honours the override: a host name that does not resolve is
// reached through its pinned address.
func TestGitConnectsToThePinnedAddress(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Repository not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	trust(t, srv)
	testEnv = append(testEnv, "GIT_SSL_NO_VERIFY=true") // the certificate is not for the pinned name
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	require.NoError(t, err)
	remote := "https://pinned.invalid:" + port + "/acme/x.git"
	u, _ := url.Parse(remote)
	rememberPin(u, []net.IPAddr{{IP: net.ParseIP(host)}})
	err = CanRead(context.Background(), remote, Auth{})
	assert.True(t, errors.Is(err, ErrNoAccess), "the request reached the server through the pin (a refusal, not a DNS failure): %v", err)
}
