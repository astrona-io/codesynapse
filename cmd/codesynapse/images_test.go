package main

import (
	"bytes"
	"compress/gzip"
	"context"
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

// TestImagesUpload: the report goes to the API gzipped with the key from the
// environment, and the answer is summarised.
func TestImagesUpload(t *testing.T) {
	t.Setenv(apiTokenEnv, "csk_test")
	var path, auth, encoding, body string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth, encoding = r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Encoding")
		zr, err := gzip.NewReader(r.Body)
		require.NoError(t, err)
		b, err := io.ReadAll(zr)
		require.NoError(t, err)
		body = string(b)
		_, _ = io.WriteString(w, `{"uploaded_at":"2026-10-25T10:00:00Z","images":["app:1"],"counts":{"critical":1},"total":1,"findings":[{"image":"app:1","package":"zlib","version":"1.2","id":"CVE-2025-0002","severity":"critical"}]}`)
	}))
	t.Cleanup(api.Close)

	report := filepath.Join(t.TempDir(), "trivy.json")
	require.NoError(t, os.WriteFile(report, []byte(`{"ArtifactName":"app:1"}`), 0o600))
	var out bytes.Buffer
	env := &app.Env{Stdout: &out, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Args: []string{report}}
	o := &imagesOptions{EC: env, upload: true, repo: "acme/demo", apiURL: api.URL}
	require.NoError(t, o.Complete(context.Background(), env))
	require.NoError(t, o.Validate(context.Background(), env))
	require.NoError(t, o.Run(context.Background(), env))

	assert.Equal(t, path, "/api/v1/repos/acme%2Fdemo/images")
	assert.Equal(t, auth, "Bearer csk_test")
	assert.Equal(t, encoding, "gzip")
	assert.Equal(t, body, `{"ArtifactName":"app:1"}`)
	assert.Contains(t, out.String(), "1 image findings from app:1")
	assert.Contains(t, out.String(), "critical CVE-2025-0002 zlib@1.2 (no fix yet)")

	none := &imagesOptions{EC: &app.Env{}, upload: true}
	assert.Error(t, none.Validate(context.Background(), none.EC))
}
