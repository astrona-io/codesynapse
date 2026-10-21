// Package forge lists the repositories of a GitHub organisation or user, so
// CodeSynapse can index and keep in sync everything an organisation owns.
// It only ever talks to api.github.com; the token is sent nowhere else.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// DefaultAPI is the GitHub REST endpoint.
const DefaultAPI = "https://api.github.com"

const (
	perPage      = 100
	maxPages     = 20       // 2000 repositories per organisation
	maxPageBytes = 16 << 20 // a page of 100 repositories is well below this
)

// ErrNotFound means the organisation or user does not exist (or the token
// cannot see it).
var ErrNotFound = errors.New("organisation not found")

// ownerRE is GitHub's login alphabet: alphanumerics and single hyphens.
var ownerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// ValidOwner reports whether s can be an organisation or user login.
func ValidOwner(s string) bool { return ownerRE.MatchString(s) }

// Repo is one repository of an organisation.
type Repo struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description,omitempty"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	Private       bool   `json:"private"`
	PushedAt      string `json:"pushed_at,omitempty"`
}

// CloneURL is the https clone URL of repo under owner. It is built here
// instead of taken from the API answer, so a response can only ever point at
// github.com/<owner>/<name>.
func CloneURL(owner, name string) string {
	return "https://github.com/" + owner + "/" + name + ".git"
}

// Client lists repositories. The zero value uses GitHub with no token.
type Client struct {
	// BaseURL overrides DefaultAPI (tests only).
	BaseURL string
	// Token is a GitHub token for private repositories; sent only to BaseURL.
	Token string
	HTTP  *http.Client
}

func (c Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		// The token must not follow a redirect to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ListRepos returns every repository of owner, an organisation or else a user.
func (c Client) ListRepos(ctx context.Context, owner string) ([]Repo, error) {
	if !ValidOwner(owner) {
		return nil, fmt.Errorf("invalid organisation name %q", owner)
	}
	repos, err := c.list(ctx, "/orgs/"+owner+"/repos", "type=all")
	if errors.Is(err, ErrNotFound) {
		repos, err = c.list(ctx, "/users/"+owner+"/repos", "type=owner")
	}
	return repos, err
}

func (c Client) list(ctx context.Context, path, query string) ([]Repo, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultAPI
	}
	var all []Repo
	for page := 1; page <= maxPages; page++ {
		u := fmt.Sprintf("%s%s?%s&per_page=%d&page=%d&sort=full_name", base, path, query, perPage, page)
		batch, err := c.page(ctx, u)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < perPage {
			break
		}
	}
	return all, nil
}

func (c Client) page(ctx context.Context, rawURL string) ([]Repo, error) {
	if _, err := url.Parse(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "codesynapse")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck // read-only
	body, err := io.ReadAll(io.LimitReader(res.Body, maxPageBytes))
	if err != nil {
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case res.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("GitHub rejected the token (401); check CODESYNAPSE_GIT_TOKEN")
	case res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests:
		return nil, errors.New("GitHub refused the request (rate limit or missing access); set CODESYNAPSE_GIT_TOKEN to raise the limit")
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub answered %s", res.Status)
	}
	var repos []Repo
	if err := json.Unmarshal(body, &repos); err != nil {
		return nil, fmt.Errorf("unexpected answer from GitHub: %w", err)
	}
	return repos, nil
}
