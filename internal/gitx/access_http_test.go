package gitx

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// gitHost serves a bare repository over TLS with git http-backend. Only
// requests with the token pass; others get status (401 like GitLab, 404
// like GitHub hides private repositories).
func gitHost(t *testing.T, token string, status int) (remote string) {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git not installed")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	root := t.TempDir()
	work := filepath.Join(root, "work")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", work},
		{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
		{"clone", "-q", "--bare", work, filepath.Join(root, "acme", "secret.git")},
	} {
		out, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			if status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			}
			http.Error(w, "Repository not found", status)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(root, "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	testEnv = []string{"GIT_SSL_CAINFO=" + ca}
	t.Cleanup(func() { testEnv = nil })
	return srv.URL + "/acme/secret.git"
}

func TestCanReadAgainstARealHost(t *testing.T) {
	ctx := context.Background()
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		remote := gitHost(t, "good-token-123", status)
		host := strings.Split(strings.Split(remote, "/")[2], ":")[0]
		assert.NoError(t, CanRead(ctx, remote, Auth{Host: host, Token: "good-token-123"}), "the right token reads")
		err := CanRead(ctx, remote, Auth{Host: host, Token: "revoked-token"})
		assert.True(t, errors.Is(err, ErrNoAccess), "a refused token is ErrNoAccess (HTTP %d): %v", status, err)
		err = CanRead(ctx, remote, Auth{})
		assert.True(t, errors.Is(err, ErrNoAccess), "anonymous is refused (HTTP %d): %v", status, err)
		dir := filepath.Join(t.TempDir(), "clone")
		require.NoError(t, Clone(ctx, remote, "main", dir, Auth{Host: host, Token: "good-token-123"}), "clones with the token")
	}
	err := CanRead(ctx, "https://127.0.0.1:1/acme/secret.git", Auth{})
	assert.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoAccess), "an unreachable host is not a refusal")
}
