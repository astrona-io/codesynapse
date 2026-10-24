package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestRuntimeUpload: the reports found in the checkout go to the API
// gzipped, with the commit and the key from the environment.
func TestRuntimeUpload(t *testing.T) {
	t.Setenv(apiTokenEnv, "csk_test")
	var got struct {
		Commit  string `json:"commit"`
		Reports []struct {
			Kind, Name, Content string
		} `json:"reports"`
	}
	var path, auth, encoding string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth, encoding = r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Encoding")
		zr, err := gzip.NewReader(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.NewDecoder(zr).Decode(&got))
		_, _ = io.WriteString(w, `{"commit":"abc1234","sources":["coverage cover.out"],"functions":3,"uncovered":1,"tests":2,"failed":0,"flaky":["demo.TestX"]}`)
	}))
	t.Cleanup(api.Close)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cover.out"), []byte("mode: set\nexample.com/demo/demo.go:3.1,4.2 1 1\n"), 0o600))
	var out bytes.Buffer
	env := &app.Env{Stdout: &out, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Args: []string{dir}}
	o := &runtimeOptions{EC: env, upload: true, repo: "acme/demo", apiURL: api.URL, commit: "abc1234"}
	require.NoError(t, o.Complete(context.Background(), env))
	require.NoError(t, o.Validate(context.Background(), env))
	require.NoError(t, o.Run(context.Background(), env))

	assert.Equal(t, path, "/api/v1/repos/acme%2Fdemo/runtime")
	assert.Equal(t, auth, "Bearer csk_test")
	assert.Equal(t, encoding, "gzip")
	assert.Equal(t, got.Commit, "abc1234")
	require.Len(t, got.Reports, 1)
	assert.Equal(t, got.Reports[0].Kind, "coverage")
	assert.Equal(t, got.Reports[0].Name, "cover.out")
	assert.Contains(t, out.String(), "3 functions measured, 1 not executed by any test")
	assert.Contains(t, out.String(), "flaky: demo.TestX")

	o = &runtimeOptions{EC: &app.Env{Args: []string{t.TempDir()}}, upload: true, repo: "acme/demo", apiURL: api.URL, commit: "abc1234"}
	require.NoError(t, o.Complete(context.Background(), o.EC))
	err := o.Run(context.Background(), o.EC)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no reports found")
}
