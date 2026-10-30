package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestValidOwner(t *testing.T) {
	for in, want := range map[string]bool{
		"astrona-io": true, "a": true, "A1-b2": true,
		"": false, "-x": false, "a/b": false, "a b": false, "..": false, strings.Repeat("a", 40): false,
	} {
		assert.Equal(t, ValidOwner(GitHub, in), want, in)
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
	c := Client{BaseURL: srv.URL, AllowPrivate: true, Token: "tok"}

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

func TestListReposAsksASlowPageAgainOnce(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		mu.Lock()
		asked[r.URL.Path+"#"+page]++
		n := asked[r.URL.Path+"#"+page]
		mu.Unlock()
		slow := (r.URL.Path == "/orgs/flaky/repos" && page == "2" && n == 1) || r.URL.Path == "/orgs/stuck/repos"
		if slow {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		count := 100
		if page == "2" {
			count = 1
		}
		names := make([]string, count)
		for i := range names {
			names[i] = fmt.Sprintf(`{"name":"r%d","default_branch":"main"}`, i)
		}
		_, _ = fmt.Fprint(w, "["+strings.Join(names, ",")+"]")
	}))
	defer srv.Close()
	times := func(key string) int {
		mu.Lock()
		defer mu.Unlock()
		return asked[key]
	}
	c := Client{BaseURL: srv.URL, AllowPrivate: true, HTTP: &http.Client{Timeout: 200 * time.Millisecond}}

	t.Run("a page that timed out once is asked again", func(t *testing.T) {
		got, err := c.ListRepos(context.Background(), "flaky")
		require.NoError(t, err)
		assert.Len(t, got, 101)
		assert.Equal(t, times("/orgs/flaky/repos#2"), 2)
	})

	t.Run("only once", func(t *testing.T) {
		_, err := c.ListRepos(context.Background(), "stuck")
		require.Error(t, err)
		assert.Equal(t, times("/orgs/stuck/repos#1"), 2)
	})

	t.Run("not when the caller's context ended", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := Client{BaseURL: srv.URL, AllowPrivate: true}.ListRepos(ctx, "stuck")
		require.Error(t, err)
		assert.Equal(t, times("/orgs/stuck/repos#1"), 3)
	})
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
	_, err := Client{BaseURL: srv.URL, AllowPrivate: true, Token: "tok"}.ListRepos(context.Background(), "x")
	assert.Error(t, err)
}

func TestCloneURL(t *testing.T) {
	assert.Equal(t, Client{}.CloneURL("astrona-io", "api"), "https://github.com/astrona-io/api.git")
	assert.Equal(t, Client{Kind: Forgejo, Host: "Git.Example.com"}.CloneURL("team", "api"), "https://git.example.com/team/api.git")
}

func TestRepoNameKeepsProvidersApart(t *testing.T) {
	assert.Equal(t, RepoName("github.com", "Astrona-IO", "api"), "astrona-io/api")
	assert.Equal(t, RepoName("", "astrona-io", "api"), "astrona-io/api")
	assert.Equal(t, RepoName("git.example.com", "Astrona-IO", "api"), "git.example.com/astrona-io/api")
	assert.NotEqual(t, RepoName("codeberg.org", "astrona-io", "api"), RepoName("github.com", "astrona-io", "api"))
}

func TestValidHostAndOwner(t *testing.T) {
	for in, want := range map[string]bool{"git.example.com": true, "codeberg.org": true, "forgejo": true,
		"": false, "Git.Example.com": false, "a b": false, "host:3000": false, "a/b": false, "-a.com": false, "u@h.com": false} {
		assert.Equal(t, ValidHost(in), want, in)
	}
	assert.True(t, ValidOwner(Forgejo, "my_team.x"))
	assert.False(t, ValidOwner(GitHub, "my_team"))
	assert.False(t, ValidOwner(Forgejo, "x.git"))
	assert.False(t, ValidOwner(GitLab, "../x"))
}

func TestForgejoAndGitLab(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"|"+r.Header.Get("Authorization")+"|"+r.Header.Get("PRIVATE-TOKEN"))
		switch r.URL.Path {
		case "/orgs/team/repos":
			if r.URL.Query().Get("page") == "1" {
				_, _ = fmt.Fprint(w, `[{"name":"api","default_branch":"main","fork":true,"private":true,"updated_at":"2026-01-02T03:04:05Z"}]`)
			} else {
				_, _ = fmt.Fprint(w, `[]`)
			}
		case "/groups/grp/projects":
			_, _ = fmt.Fprint(w, `[{"path":"svc","default_branch":"dev","archived":true,"visibility":"private","forked_from_project":{"id":1},"last_activity_at":"2026-02-03T00:00:00Z"},{"path":"pub","visibility":"public","forked_from_project":null}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := Client{Kind: Forgejo, Host: "git.example.com", BaseURL: srv.URL, AllowPrivate: true, Token: "tok"}.ListRepos(context.Background(), "team")
	require.NoError(t, err)
	require.Len(t, got, 1, "a short page is not the last one")
	assert.Equal(t, got[0].Name, "api")
	assert.True(t, got[0].Fork)
	assert.True(t, got[0].Private)
	assert.Equal(t, got[0].PushedAt, "2026-01-02T03:04:05Z")
	assert.Equal(t, seen[0], "/orgs/team/repos|token tok|")

	got, err = Client{Kind: GitLab, Host: "gitlab.example.com", BaseURL: srv.URL, AllowPrivate: true, Token: "tok"}.ListRepos(context.Background(), "grp")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, got[0].Name, "svc")
	assert.True(t, got[0].Archived)
	assert.True(t, got[0].Fork)
	assert.False(t, got[1].Fork)
	assert.False(t, got[1].Private)
	assert.Equal(t, seen[len(seen)-1], "/groups/grp/projects||tok")
}

func TestAPIBase(t *testing.T) {
	assert.Equal(t, Client{}.apiBase(), "https://api.github.com")
	assert.Equal(t, Client{Host: "ghe.corp"}.apiBase(), "https://ghe.corp/api/v3")
	assert.Equal(t, Client{Kind: Forgejo, Host: "codeberg.org"}.apiBase(), "https://codeberg.org/api/v1")
	assert.Equal(t, Client{Kind: GitLab, Host: "gitlab.com"}.apiBase(), "https://gitlab.com/api/v4")
}

func TestKinds(t *testing.T) {
	k, ok := ParseKind("Gitea")
	assert.True(t, ok)
	assert.Equal(t, k, Forgejo)
	_, ok = ParseKind("bitbucket")
	assert.False(t, ok)
	k, ok = KindForHost("codeberg.org")
	assert.True(t, ok)
	assert.Equal(t, k, Forgejo)
	_, ok = KindForHost("git.example.com")
	assert.False(t, ok)
}

func TestRepoExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/repos/o/here", "/projects/grp%2Fhere":
			_, _ = fmt.Fprint(w, `{}`)
		case "/repos/o/denied":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	gh := Client{BaseURL: srv.URL, AllowPrivate: true}
	ok, err := gh.RepoExists(context.Background(), "o", "here")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = gh.RepoExists(context.Background(), "o", "gone")
	require.NoError(t, err)
	assert.False(t, ok, "a clean 404 means gone")
	_, err = gh.RepoExists(context.Background(), "o", "denied")
	assert.Error(t, err, "a refusal is not proof the repository is gone")
	_, err = gh.RepoExists(context.Background(), "o", "../x")
	assert.Error(t, err)
	ok, err = Client{Kind: GitLab, BaseURL: srv.URL, AllowPrivate: true}.RepoExists(context.Background(), "grp", "here")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestListReposReadsPermissionsAndNeverTheTokenOwnersRepositories(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Query().Get("page") > "1" {
			_, _ = fmt.Fprint(w, `[]`) // Forgejo reads until an empty page
			return
		}
		switch r.URL.Path {
		case "/user":
			_, _ = fmt.Fprint(w, `{"login":"Me"}`)
		case "/user/repos":
			_, _ = fmt.Fprint(w, `[{"name":"secret","private":true,"permissions":{"admin":true}}]`)
		case "/users/me/repos":
			_, _ = fmt.Fprint(w, `[{"name":"mine","permissions":{"admin":true}}]`)
		case "/users/other/repos":
			_, _ = fmt.Fprint(w, `[{"name":"pub","permissions":{"pull":true}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	for _, kind := range []Kind{GitHub, Forgejo} {
		paths = nil
		c := Client{Kind: kind, BaseURL: srv.URL, AllowPrivate: true, Token: "tok"}
		got, err := c.ListRepos(context.Background(), "me")
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, got[0].Name, "mine", "the token owner's login lists only the public user endpoint")
		assert.Equal(t, got[0].Permission, PermAdmin)
		for _, p := range paths {
			assert.True(t, !strings.HasPrefix(p, "/user/") && p != "/user", "never asks about the token's owner: "+p)
		}
	}
	got, err := Client{BaseURL: srv.URL, AllowPrivate: true, Token: "tok"}.ListRepos(context.Background(), "other")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, got[0].Permission, PermRead)
}

func TestClientRefusesInternalAddressesWhenItConnects(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	_, err := Client{BaseURL: srv.URL, Token: "tok"}.ListRepos(context.Background(), "x")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInternalAddress), err.Error())
	assert.Len(t, auth, 0, "the token never reached the internal address")
	_, err = Client{BaseURL: srv.URL, AllowPrivate: true, Token: "tok"}.ListRepos(context.Background(), "x")
	require.NoError(t, err, "allowed with CODESYNAPSE_ALLOW_PRIVATE_GIT")
	assert.Len(t, auth, 1)
}

func TestListReposExplainsSAMLRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GitHub-SSO", "required; url=https://github.com/orgs/x/sso")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := Client{BaseURL: srv.URL, AllowPrivate: true, Token: "tok"}.ListRepos(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single sign-on")
}

func TestGitLabPermission(t *testing.T) {
	for level, want := range map[int]string{50: PermAdmin, 40: PermAdmin, 30: PermWrite, 20: PermRead, 10: PermRead, 0: ""} {
		assert.Equal(t, gitlabPermission(level), want)
	}
}
