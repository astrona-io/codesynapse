package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestValidOwner(t *testing.T) {
	for in, want := range map[string]bool{
		"astrona-io": true, "a": true, "A1-b2": true,
		"": false, "-x": false, "a/b": false, "a b": false, "..": false, strings.Repeat("a", 40): false,
	} {
		assert.Equal(t, ValidOwner(in), want, in)
	}
}

func TestListReposPagesAndFallsBackToUser(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/orgs/big/repos":
			n := 100
			if r.URL.Query().Get("page") == "2" {
				n = 3
			}
			_, _ = fmt.Fprint(w, "[")
			for i := 0; i < n; i++ {
				if i > 0 {
					_, _ = fmt.Fprint(w, ",")
				}
				_, _ = fmt.Fprintf(w, `{"name":"r%d","default_branch":"main"}`, i)
			}
			_, _ = fmt.Fprint(w, "]")
		case "/users/me/repos":
			_, _ = fmt.Fprint(w, `[{"name":"mine","default_branch":"dev","archived":true}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := Client{BaseURL: srv.URL, Token: "tok"}

	got, err := c.ListRepos(context.Background(), "big")
	require.NoError(t, err)
	assert.Len(t, got, 103)

	got, err = c.ListRepos(context.Background(), "me")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, got[0].DefaultBranch, "dev")
	assert.True(t, got[0].Archived)
	assert.Equal(t, auth[0], "Bearer tok")

	_, err = c.ListRepos(context.Background(), "nobody")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = c.ListRepos(context.Background(), "../etc")
	assert.Error(t, err)
}

func TestListReposDoesNotFollowRedirects(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("token must not reach another host")
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	_, err := Client{BaseURL: srv.URL, Token: "tok"}.ListRepos(context.Background(), "x")
	assert.Error(t, err)
}

func TestCloneURL(t *testing.T) {
	assert.Equal(t, CloneURL("astrona-io", "api"), "https://github.com/astrona-io/api.git")
}
