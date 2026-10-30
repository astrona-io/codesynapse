package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/gatewayproxy"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const (
	proxyTestKey  = "csk_proxytestkey"
	proxyTestHook = "'/usr/local/bin/codesynapse' gateway proxy --ensure"
)

// runningProxy starts our proxy for cfg on a free loopback port and returns
// the config with that port.
func runningProxy(t *testing.T, files gatewayproxy.Files, gateway string) gatewayproxy.Config {
	t.Helper()
	return runningProxyOn(t, files, gatewayproxy.Config{Gateway: gateway, Port: freePort(t)})
}

func runningProxyOn(t *testing.T, files gatewayproxy.Files, cfg gatewayproxy.Config) gatewayproxy.Config {
	t.Helper()
	ln, err := gatewayproxy.Listen(t.Context(), net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)))
	require.NoError(t, err)
	require.NoError(t, files.Save(cfg))
	secret, err := files.Secret()
	require.NoError(t, err)
	p, err := gatewayproxy.New(gatewayproxy.Options{Config: cfg, Files: files, Secret: secret})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cfg
}

func strangerPort(t *testing.T) int {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) }))
	t.Cleanup(s.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	n, _ := strconv.Atoi(port)
	return n
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func settingsBase(t *testing.T, file string) string {
	t.Helper()
	base, _, ok := agentcfg.ClaudeGatewayConfig(file)
	require.True(t, ok)
	return base
}

func TestInstallGatewayProxy(t *testing.T) {
	const gw = "https://gw.example.com"
	noStart := func() error { return errors.New("not in tests") }
	t.Run("our proxy runs", func(t *testing.T) {
		files := gatewayproxy.Files{Dir: t.TempDir()}
		cfg := runningProxy(t, files, gw)
		settings := filepath.Join(t.TempDir(), "settings.json")
		diff, err := installGatewayProxy(t.Context(), files, cfg, settings, proxyTestKey, proxyTestHook, noStart)
		require.NoError(t, err)
		assert.Contains(t, diff, cfg.URL())
		assert.NotContains(t, diff, proxyTestKey)
		assert.Equal(t, settingsBase(t, settings), cfg.URL())
		b, err := os.ReadFile(settings)
		require.NoError(t, err)
		assert.Contains(t, string(b), "gateway proxy --ensure")
	})
	t.Run("port taken by a stranger", func(t *testing.T) {
		files := gatewayproxy.Files{Dir: t.TempDir()}
		settings := filepath.Join(t.TempDir(), "settings.json")
		cfg := gatewayproxy.Config{Gateway: gw, Port: strangerPort(t)}
		_, err := installGatewayProxy(t.Context(), files, cfg, settings, proxyTestKey, proxyTestHook, noStart)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--proxy-port")
		_, statErr := os.Stat(settings)
		assert.True(t, errors.Is(statErr, os.ErrNotExist), "settings are not pointed at a stranger")
		_, err = files.Load()
		assert.ErrorIs(t, err, gatewayproxy.ErrNotConfigured)
	})
	t.Run("proxy does not start", func(t *testing.T) {
		files := gatewayproxy.Files{Dir: t.TempDir()}
		settings := filepath.Join(t.TempDir(), "settings.json")
		cfg := gatewayproxy.Config{Gateway: gw, Port: freePort(t)}
		_, err := installGatewayProxy(t.Context(), files, cfg, settings, proxyTestKey, proxyTestHook, noStart)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--failover=false")
	})
}

func TestEnsureProxy(t *testing.T) {
	const gw = "https://gw.example.com"
	cases := []struct {
		name     string
		port     func(t *testing.T, files gatewayproxy.Files) gatewayproxy.Config
		wantBase func(cfg gatewayproxy.Config) string
		wantMsg  string
	}{
		{
			name:     "our proxy runs",
			port:     func(t *testing.T, files gatewayproxy.Files) gatewayproxy.Config { return runningProxy(t, files, gw) },
			wantBase: func(cfg gatewayproxy.Config) string { return cfg.URL() },
		},
		{
			name: "port taken by a stranger",
			port: func(t *testing.T, files gatewayproxy.Files) gatewayproxy.Config {
				cfg := gatewayproxy.Config{Gateway: gw, Port: strangerPort(t)}
				require.NoError(t, files.Save(cfg))
				return cfg
			},
			wantBase: func(gatewayproxy.Config) string { return gw },
			wantMsg:  "another program holds port",
		},
		{
			name: "proxy does not start",
			port: func(t *testing.T, files gatewayproxy.Files) gatewayproxy.Config {
				cfg := gatewayproxy.Config{Gateway: gw, Port: freePort(t)}
				require.NoError(t, files.Save(cfg))
				return cfg
			},
			wantBase: func(gatewayproxy.Config) string { return gw },
			wantMsg:  "did not start",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := gatewayproxy.Files{Dir: t.TempDir()}
			cfg := tc.port(t, files)
			settings := filepath.Join(t.TempDir(), "settings.json")
			_, err := agentcfg.SetClaudeGatewayProxy(settings, cfg.URL(), proxyTestKey, proxyTestHook, time.Now())
			require.NoError(t, err)
			var out bytes.Buffer
			ensureProxy(t.Context(), files, cfg, settings, func() error { return errors.New("not in tests") }, &out)
			assert.Equal(t, settingsBase(t, settings), tc.wantBase(cfg))
			if tc.wantMsg == "" {
				assert.Equal(t, out.String(), "")
				return
			}
			assert.Contains(t, out.String(), `"systemMessage"`)
			assert.Contains(t, out.String(), tc.wantMsg)
			assert.NotContains(t, out.String(), proxyTestKey)

			// Once the proxy runs again, the next hook points back at it.
			if tc.name == "proxy does not start" {
				running := runningProxyOn(t, files, cfg)
				out.Reset()
				ensureProxy(t.Context(), files, running, settings, func() error { return errors.New("not in tests") }, &out)
				assert.Equal(t, settingsBase(t, settings), running.URL())
			}
		})
	}
}
