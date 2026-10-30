package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/fetchsource"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const cachedPage = "https://docs.example.com/guide?b=2&a=1"

// steerEnv gives the hook its own state and user cache folders (HOME for
// macOS, XDG_CACHE_HOME for Linux) and returns the source cache folder,
// which does not exist yet.
func steerEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("CODESYNAPSE_FETCH_STEER", "")
	dir, err := fetchsource.DefaultDir()
	require.NoError(t, err)
	return dir
}

// cachePage stores raw in the source cache the way fetch_source does.
func cachePage(t *testing.T, dir, raw string) {
	t.Helper()
	c, err := fetchsource.OpenCache(dir, 0)
	require.NoError(t, err)
	u, err := fetchsource.Normalise(raw)
	require.NoError(t, err)
	require.NoError(t, c.Put(fetchsource.Key(u), fetchsource.Entry{URL: u.String(), Fetched: time.Now(), TTL: time.Hour, Text: "page"}))
}

func webFetch(session, url string) toolHookInput {
	in := toolHookInput{SessionID: session, ToolName: "WebFetch"}
	in.ToolInput.URL = url
	return in
}

func steerReason(t *testing.T, out map[string]any) string {
	t.Helper()
	require.True(t, out != nil, "steered")
	h := out["hookSpecificOutput"].(map[string]any)
	assert.Equal(t, h["permissionDecision"], "deny")
	return h["permissionDecisionReason"].(string)
}

func TestFetchSteer(t *testing.T) {
	dir := steerEnv(t)
	cachePage(t, dir, cachedPage)

	// Another spelling of the same page has the same cache key.
	reason := steerReason(t, fetchSteer(webFetch("chat-1", "https://DOCS.example.com:443/guide?a=1&b=2&utm_source=x#top")))
	assert.Contains(t, reason, "fetch_source")
	assert.Contains(t, reason, `"https://docs.example.com/guide?a=1&b=2"`)
	assert.Contains(t, reason, "question")
	assert.Contains(t, reason, "call WebFetch again")
	assert.True(t, fetchSteer(webFetch("chat-1", cachedPage)) == nil, "the second WebFetch of a steered page goes through")
	assert.True(t, fetchSteer(webFetch("chat-2", cachedPage)) != nil, "another session is steered once too")

	s := loadState(stateFile("chat-1"))
	assert.Equal(t, s.Steers, 1)
	for _, k := range s.Steered {
		assert.False(t, strings.Contains(k, "example"), "state keeps the cache key, not the URL")
	}
	rep := s.workflowReport()
	assert.Equal(t, rep["fetches_steered"], 1)
	s.markReported()
	assert.True(t, s.workflowReport() == nil, "nothing new after a report")
}

func TestFetchSteerLetsThrough(t *testing.T) {
	dir := steerEnv(t)
	cachePage(t, dir, cachedPage)
	tests := []struct {
		name string
		in   toolHookInput
	}{
		{"uncached https page", webFetch("chat-1", "https://docs.example.com/other")},
		{"http, even when the https page is cached", webFetch("chat-1", "http://docs.example.com/guide?a=1&b=2")},
		{"malformed URL", webFetch("chat-1", "https://docs.example.com/%zz")},
		{"no host", webFetch("chat-1", "https:///guide")},
		{"credentials in the URL", webFetch("chat-1", "https://user:pw@docs.example.com/guide?a=1&b=2")},
		{"no URL", webFetch("chat-1", "")},
		{"not a plain session ID", webFetch("../x", cachedPage)},
		{"another tool", func() toolHookInput { in := webFetch("chat-1", cachedPage); in.ToolName = "WebSearch"; return in }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.True(t, fetchSteer(tt.in) == nil) })
	}
	assert.Equal(t, loadState(stateFile("chat-1")).Steers, 0)
}

func TestFetchSteerWithoutCache(t *testing.T) {
	dir := steerEnv(t)
	assert.True(t, fetchSteer(webFetch("chat-1", cachedPage)) == nil, "no cache folder")
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "the hook never creates the cache folder")
}

func TestFetchSteerDisabled(t *testing.T) {
	dir := steerEnv(t)
	cachePage(t, dir, cachedPage)
	t.Setenv("CODESYNAPSE_FETCH_STEER", "false")
	assert.True(t, fetchSteer(webFetch("chat-1", cachedPage)) == nil)
}

// The command exits 0 with no output for anything it has no opinion on, and
// logs nothing about the URL either way.
func TestFetchSteerCommand(t *testing.T) {
	dir := steerEnv(t)
	cachePage(t, dir, cachedPage)
	var out, logs bytes.Buffer
	run := func(stdin string) string {
		out.Reset()
		env := &app.Env{Stdin: strings.NewReader(stdin), Stdout: &out, Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
		require.NoError(t, (&hookOptions{EC: env, event: "webfetch"}).Run(context.Background(), nil))
		return out.String()
	}
	payload := func(url string) string {
		raw, err := json.Marshal(map[string]any{"session_id": "chat-7", "tool_name": "WebFetch", "tool_input": map[string]string{"url": url, "prompt": "what changed?"}})
		require.NoError(t, err)
		return string(raw)
	}
	assert.Contains(t, run(payload(cachedPage)), `"permissionDecision":"deny"`)
	assert.Equal(t, run(payload(cachedPage)), "", "second fetch goes through")
	assert.Equal(t, run(payload("https://docs.example.com/new")), "")
	assert.Equal(t, run(`not json`), "")
	assert.Equal(t, run(``), "")
	assert.False(t, strings.Contains(logs.String(), "example.com"), "the URL is never logged")
}
