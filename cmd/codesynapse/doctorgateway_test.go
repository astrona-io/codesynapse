package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/gatewayproxy"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestGatewayCheck(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+proxyTestKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(gw.Close)
	noStart := func() error { return errors.New("not in tests") }
	hash := gatewayproxy.HashKey(proxyTestKey)

	cases := []struct {
		name string
		// setup writes settings and proxy files; it returns the proxy config
		// the fix is expected to install, if any.
		setup      func(t *testing.T, settings string, files gatewayproxy.Files) *gatewayproxy.Config
		wantOK     bool
		wantDetail string
		wantFix    bool
	}{
		{
			name:       "no gateway",
			setup:      func(*testing.T, string, gatewayproxy.Files) *gatewayproxy.Config { return nil },
			wantOK:     true,
			wantDetail: "no gateway set up",
		},
		{
			name: "gateway without failover from before it existed",
			setup: func(t *testing.T, settings string, _ gatewayproxy.Files) *gatewayproxy.Config {
				_, err := agentcfg.SetClaudeGateway(settings, gw.URL, proxyTestKey, time.Now())
				require.NoError(t, err)
				return &gatewayproxy.Config{Gateway: gw.URL, Port: gatewayproxy.DefaultPort, KeyHash: hash}
			},
			wantDetail: "directly",
			wantFix:    true,
		},
		{
			name: "failover turned off on purpose",
			setup: func(t *testing.T, settings string, files gatewayproxy.Files) *gatewayproxy.Config {
				_, err := agentcfg.SetClaudeGateway(settings, gw.URL, proxyTestKey, time.Now())
				require.NoError(t, err)
				require.NoError(t, files.ChooseDirect())
				return nil
			},
			wantOK:     true,
			wantDetail: "--failover=false",
		},
		{
			name: "proxy config missing behind the hooks",
			setup: func(t *testing.T, settings string, _ gatewayproxy.Files) *gatewayproxy.Config {
				_, err := agentcfg.SetClaudeGatewayProxy(settings, "http://127.0.0.1:18431", proxyTestKey, proxyTestHook, time.Now())
				require.NoError(t, err)
				return nil
			},
			wantDetail: "config is missing",
		},
		{
			name: "proxy holds another key",
			setup: func(t *testing.T, settings string, files gatewayproxy.Files) *gatewayproxy.Config {
				cfg := gatewayproxy.Config{Gateway: gw.URL, Port: freePort(t), KeyHash: gatewayproxy.HashKey("csk_other")}
				require.NoError(t, files.Save(cfg))
				_, err := agentcfg.SetClaudeGatewayProxy(settings, cfg.URL(), proxyTestKey, proxyTestHook, time.Now())
				require.NoError(t, err)
				cfg.KeyHash = hash
				return &cfg
			},
			wantDetail: "another key",
			wantFix:    true,
		},
		{
			name: "hooks missing",
			setup: func(t *testing.T, settings string, files gatewayproxy.Files) *gatewayproxy.Config {
				cfg := gatewayproxy.Config{Gateway: gw.URL, Port: freePort(t), KeyHash: hash}
				require.NoError(t, files.Save(cfg))
				_, err := agentcfg.SetClaudeGateway(settings, cfg.URL(), proxyTestKey, time.Now())
				require.NoError(t, err)
				return &cfg
			},
			wantDetail: "hooks",
			wantFix:    true,
		},
		{
			name: "proxy idle",
			setup: func(t *testing.T, settings string, files gatewayproxy.Files) *gatewayproxy.Config {
				cfg := gatewayproxy.Config{Gateway: gw.URL, Port: freePort(t), KeyHash: hash}
				require.NoError(t, files.Save(cfg))
				_, err := agentcfg.SetClaudeGatewayProxy(settings, cfg.URL(), proxyTestKey, proxyTestHook, time.Now())
				require.NoError(t, err)
				return nil
			},
			wantOK:     true,
			wantDetail: "idle",
		},
		{
			name: "proxy runs",
			setup: func(t *testing.T, settings string, files gatewayproxy.Files) *gatewayproxy.Config {
				cfg := runningProxyOn(t, files, gatewayproxy.Config{Gateway: gw.URL, Port: freePort(t), KeyHash: hash})
				_, err := agentcfg.SetClaudeGatewayProxy(settings, cfg.URL(), proxyTestKey, proxyTestHook, time.Now())
				require.NoError(t, err)
				return nil
			},
			wantOK:     true,
			wantDetail: "runs",
		},
		{
			name: "port held by a stranger",
			setup: func(t *testing.T, settings string, files gatewayproxy.Files) *gatewayproxy.Config {
				cfg := gatewayproxy.Config{Gateway: gw.URL, Port: strangerPort(t), KeyHash: hash}
				require.NoError(t, files.Save(cfg))
				_, err := agentcfg.SetClaudeGatewayProxy(settings, cfg.URL(), proxyTestKey, proxyTestHook, time.Now())
				require.NoError(t, err)
				return nil
			},
			wantDetail: "another program holds port",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := filepath.Join(t.TempDir(), "settings.json")
			files := gatewayproxy.Files{Dir: t.TempDir()}
			want := tc.setup(t, settings, files)
			var installed *gatewayproxy.Config
			install := func(_ context.Context, _ gatewayproxy.Files, cfg gatewayproxy.Config, file, key string) (string, error) {
				assert.Equal(t, file, settings)
				assert.Equal(t, key, proxyTestKey)
				installed = &cfg
				return "", nil
			}
			c := gatewayCheck(t.Context(), settings, files, install, noStart)
			assert.Equal(t, c.OK, tc.wantOK)
			assert.Contains(t, c.Detail, tc.wantDetail)
			assert.NotContains(t, c.Detail, proxyTestKey)
			assert.Equal(t, c.fix != nil, tc.wantFix)
			if !tc.wantOK {
				assert.NotEqual(t, c.Action, "")
			}
			if c.fix == nil {
				return
			}
			require.NoError(t, c.fix(t.Context()))
			require.True(t, installed != nil, "the fix installs the proxy")
			assert.Equal(t, *installed, *want)
		})
	}
}

// A setup that fell back to the gateway because the proxy did not start is
// pointed back at the proxy once --fix gets it running.
func TestGatewayCheckFellBack(t *testing.T) {
	const gw = "https://gw.example.com"
	settings := filepath.Join(t.TempDir(), "settings.json")
	files := gatewayproxy.Files{Dir: t.TempDir()}
	cfg := gatewayproxy.Config{Gateway: gw, Port: freePort(t), KeyHash: gatewayproxy.HashKey(proxyTestKey)}
	require.NoError(t, files.Save(cfg))
	_, err := agentcfg.SetClaudeGatewayProxy(settings, gw, proxyTestKey, proxyTestHook, time.Now())
	require.NoError(t, err)
	noInstall := func(context.Context, gatewayproxy.Files, gatewayproxy.Config, string, string) (string, error) {
		t.Fatal("a fallback is repaired by starting the proxy, not by installing again")
		return "", nil
	}

	c := gatewayCheck(t.Context(), settings, files, noInstall, func() error { return errors.New("not in tests") })
	assert.False(t, c.OK)
	assert.Contains(t, c.Detail, "fell back")
	require.True(t, c.fix != nil)
	assert.Error(t, c.fix(t.Context()))
	assert.Equal(t, settingsBase(t, settings), gw)

	start := func() error { runningProxyOn(t, files, cfg); return nil }
	c = gatewayCheck(t.Context(), settings, files, noInstall, start)
	require.NoError(t, c.fix(t.Context()))
	assert.Equal(t, settingsBase(t, settings), cfg.URL())
}
