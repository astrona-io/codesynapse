// Package forge lists the repositories of an organisation (or user) on a git
// host, so CodeSynapse can index and keep in sync everything an organisation
// owns. It speaks the REST APIs of GitHub (also Enterprise Server), Forgejo
// (also Gitea, Codeberg) and GitLab. Each client talks to one host only; the
// token is sent nowhere else and redirects are never followed.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/astrona-io/codesynapse/internal/gitx"
)

// Kind names a git provider.
type Kind string

// Supported providers.
const (
	GitHub  Kind = "github"
	Forgejo Kind = "forgejo" // Gitea and Codeberg speak the same API
	GitLab  Kind = "gitlab"
)

// DefaultHost is where repositories live when no host is given.
const DefaultHost = "github.com"

// DefaultAPI is the GitHub REST endpoint.
const DefaultAPI = "https://api.github.com"

const (
	maxPages     = 20       // 2000 repositories per organisation
	maxPageBytes = 16 << 20 // a page of 100 repositories is well below this
)

// ErrNotFound means the organisation or user does not exist (or the token
// cannot see it).
var ErrNotFound = errors.New("organisation not found")

// ErrInternalAddress refuses a connection to a loopback, private or
// link-local address when the client does not allow private hosts.
var ErrInternalAddress = errors.New("the git host resolves to an internal address")

// ErrSSORequired means GitHub refused the token because the organisation
// enforces SAML single sign-on and the token is not authorised for it.
var ErrSSORequired = errors.New("token not authorised for SAML single sign-on")

// ParseKind accepts a provider name; "gitea" is an alias of "forgejo".
func ParseKind(s string) (Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "github":
		return GitHub, true
	case "forgejo", "gitea", "codeberg":
		return Forgejo, true
	case "gitlab":
		return GitLab, true
	}
	return "", false
}

// KindForHost guesses the provider of the well-known public hosts.
func KindForHost(host string) (Kind, bool) {
	switch strings.ToLower(host) {
	case "github.com":
		return GitHub, true
	case "gitlab.com":
		return GitLab, true
	case "codeberg.org":
		return Forgejo, true
	}
	return "", false
}

var (
	// githubOwnerRE is GitHub's login alphabet: alphanumerics and single hyphens.
	githubOwnerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	// otherOwnerRE is the looser alphabet of Forgejo and GitLab (dots, underscores).
	otherOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	hostRE       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)
)

// ValidOwner reports whether s can be an organisation or user login on k.
func ValidOwner(k Kind, s string) bool {
	if strings.HasSuffix(s, ".git") || strings.HasSuffix(s, ".") {
		return false
	}
	if k == GitHub {
		return githubOwnerRE.MatchString(s)
	}
	return otherOwnerRE.MatchString(s)
}

// ValidHost reports whether s is a plain lower-case host name: no scheme,
// port, path or credentials, so it is safe in a URL and in a directory name.
func ValidHost(s string) bool { return len(s) <= 253 && hostRE.MatchString(s) }

// RepoName is the identity of a repository in the workspace. github.com keeps
// the short "owner/name"; every other host is prefixed, "host/owner/name", so
// the same owner and name on two providers never collide.
func RepoName(host, owner, name string) string {
	owner = strings.ToLower(owner)
	if host == "" || strings.EqualFold(host, DefaultHost) {
		return owner + "/" + name
	}
	return strings.ToLower(host) + "/" + owner + "/" + name
}

// NameFromRemote names a repository after its https remote, as the API
// does when it clones one: "owner/name" on github.com, "host/owner/name"
// anywhere else (lower-case owner and host), "name" when the URL has no
// owner, and "" when it has a port or a deeper path.
func NameFromRemote(remote string) string {
	u, err := url.Parse(remote)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if u.Port() != "" {
		return "" // a host name alone would not tell two servers apart; name it explicitly
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git"), "/"), "/")
	switch {
	case len(parts) == 2 && (host == DefaultHost || ValidHost(host)):
		return RepoName(host, parts[0], parts[1])
	case len(parts) == 1:
		return parts[0]
	}
	return ""
}

// Repo is one repository of an organisation.
type Repo struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description,omitempty"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	Private       bool   `json:"private"`
	PushedAt      string `json:"pushed_at,omitempty"`
	// Permission is what the token may do there: "admin", "write" or
	// "read"; empty when the listing was anonymous.
	Permission string `json:"-"`
}

// Listing is the answer of ListReposDetail.
type Listing struct {
	Repos []Repo
	// SSORestricted says GitHub left out what the token is not authorised
	// for under the organisation's SAML single sign-on.
	SSORestricted bool
}

// Permission levels of Repo.Permission.
const (
	PermAdmin = "admin"
	PermWrite = "write"
	PermRead  = "read"
)

// Client lists repositories on one host. The zero value is GitHub with no token.
type Client struct {
	Kind Kind
	// Host is the git host (default github.com). The API address is derived
	// from it: api.github.com, or <host>/api/v3, /api/v1, /api/v4.
	Host string
	// BaseURL overrides the derived API address (tests only).
	BaseURL string
	// Token is for private repositories; sent only to this host.
	Token string
	// AllowPrivate permits connections to loopback, private and link-local
	// addresses (CODESYNAPSE_ALLOW_PRIVATE_GIT). Without it the default
	// client refuses them when it connects, so a DNS answer that changed
	// after the host was checked cannot carry the token inside.
	AllowPrivate bool
	HTTP         *http.Client
}

func (c Client) kind() Kind {
	if c.Kind == "" {
		return GitHub
	}
	return c.Kind
}

func (c Client) host() string {
	if c.Host == "" {
		return DefaultHost
	}
	return strings.ToLower(c.Host)
}

func (c Client) apiBase() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	switch c.kind() {
	case Forgejo:
		return "https://" + c.host() + "/api/v1"
	case GitLab:
		return "https://" + c.host() + "/api/v4"
	}
	if c.host() == DefaultHost {
		return DefaultAPI
	}
	return "https://" + c.host() + "/api/v3"
}

// CloneURL is the https clone URL of repo under owner. It is built here
// instead of taken from the API answer, so a response can only ever point at
// <host>/<owner>/<name>.
func (c Client) CloneURL(owner, name string) string {
	return "https://" + c.host() + "/" + owner + "/" + name + ".git"
}

func (c Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !c.AllowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || gitx.InternalIP(ip) {
				return ErrInternalAddress
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: time.Minute},
		// The token must not follow a redirect to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ListRepos returns every repository of owner, an organisation (group) or
// else a user.
func (c Client) ListRepos(ctx context.Context, owner string) ([]Repo, error) {
	l, err := c.ListReposDetail(ctx, owner)
	return l.Repos, err
}

// ListReposDetail is ListRepos with what the provider said about the
// token's reach.
func (c Client) ListReposDetail(ctx context.Context, owner string) (Listing, error) {
	if !ValidOwner(c.kind(), owner) {
		return Listing{}, fmt.Errorf("invalid organisation name %q", owner)
	}
	var orgPath, userPath, query string
	switch c.kind() {
	case Forgejo:
		orgPath, userPath, query = "/orgs/"+owner+"/repos", "/users/"+owner+"/repos", ""
	case GitLab:
		orgPath, userPath, query = "/groups/"+owner+"/projects", "/users/"+owner+"/projects", "include_subgroups=false"
	default:
		orgPath, userPath, query = "/orgs/"+owner+"/repos", "/users/"+owner+"/repos", "type=all"
	}
	l, err := c.list(ctx, orgPath, query)
	if errors.Is(err, ErrNotFound) {
		if c.kind() == GitHub {
			query = "type=owner"
		}
		// Always the public user endpoint, never /user/repos: the token may
		// belong to someone other than the person asking, and listing the
		// token owner's own private repositories would show them to that
		// person.
		l, err = c.list(ctx, userPath, query)
	}
	return l, err
}

func (c Client) pageSize() int {
	if c.kind() == Forgejo {
		return 50 // the default cap of Gitea and Forgejo
	}
	return 100
}

func (c Client) list(ctx context.Context, path, query string) (Listing, error) {
	var l Listing
	size := c.pageSize()
	for page := 1; page <= maxPages; page++ {
		sep := ""
		if query != "" {
			sep = "&"
		}
		q := fmt.Sprintf("%s%s%s=%d&page=%d", query, sep, c.sizeParam(), size, page)
		if c.kind() == GitHub {
			q += "&sort=full_name"
		}
		body, h, err := c.getHeader(ctx, c.apiBase()+path+"?"+q)
		if err != nil {
			return Listing{}, err
		}
		if c.kind() == GitHub && h.Get("X-GitHub-SSO") != "" {
			l.SSORestricted = true
		}
		batch, err := c.decode(body)
		if err != nil {
			return Listing{}, err
		}
		l.Repos = append(l.Repos, batch...)
		// Gitea and Forgejo admins can lower the page cap below what we ask
		// for, so a short page does not mean the last one there.
		if len(batch) == 0 || (c.kind() != Forgejo && len(batch) < size) {
			break
		}
	}
	return l, nil
}

func (c Client) sizeParam() string {
	if c.kind() == Forgejo {
		return "limit"
	}
	return "per_page"
}

func (c Client) label() string {
	switch c.kind() {
	case Forgejo:
		return "Forgejo"
	case GitLab:
		return "GitLab"
	}
	return "GitHub"
}

func (c Client) get(ctx context.Context, rawURL string) ([]byte, error) {
	body, _, err := c.getHeader(ctx, rawURL)
	return body, err
}

func (c Client) getHeader(ctx context.Context, rawURL string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "codesynapse")
	switch c.kind() {
	case GitHub:
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
	case Forgejo:
		req.Header.Set("Accept", "application/json")
		if c.Token != "" {
			req.Header.Set("Authorization", "token "+c.Token)
		}
	case GitLab:
		req.Header.Set("Accept", "application/json")
		if c.Token != "" {
			req.Header.Set("PRIVATE-TOKEN", c.Token)
		}
	}
	res, err := c.http().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("listing repositories: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck // read-only
	body, err := io.ReadAll(io.LimitReader(res.Body, maxPageBytes))
	if err != nil {
		return nil, nil, err
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return nil, nil, ErrNotFound
	case res.StatusCode == http.StatusUnauthorized:
		return nil, nil, fmt.Errorf("%s rejected the token (401); check the token configured for %s", c.label(), c.host())
	case res.StatusCode == http.StatusForbidden && c.kind() == GitHub && res.Header.Get("X-GitHub-SSO") != "":
		return nil, nil, fmt.Errorf("%w: the organisation uses SAML single sign-on and the token for %s is not authorised for it; authorise the token for the organisation in GitHub's token settings", ErrSSORequired, c.host())
	case res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests:
		return nil, nil, fmt.Errorf("%s refused the request (rate limit or missing access); configure a token for %s", c.label(), c.host())
	case res.StatusCode != http.StatusOK:
		return nil, nil, fmt.Errorf("%s answered %s", c.label(), res.Status)
	}
	return body, res.Header, nil
}

// decode maps one page of the provider's answer to Repo values.
func (c Client) decode(body []byte) ([]Repo, error) {
	bad := func(err error) ([]Repo, error) {
		return nil, fmt.Errorf("unexpected answer from %s: %w", c.label(), err)
	}
	switch c.kind() {
	case GitLab:
		var in []struct {
			Path           string          `json:"path"`
			DefaultBranch  string          `json:"default_branch"`
			Description    string          `json:"description"`
			Archived       bool            `json:"archived"`
			ForkedFrom     json.RawMessage `json:"forked_from_project"`
			Visibility     string          `json:"visibility"`
			LastActivityAt string          `json:"last_activity_at"`
			Permissions    struct {
				Project *struct {
					Level int `json:"access_level"`
				} `json:"project_access"`
				Group *struct {
					Level int `json:"access_level"`
				} `json:"group_access"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return bad(err)
		}
		out := make([]Repo, 0, len(in))
		for _, r := range in {
			level := 0
			if p := r.Permissions.Project; p != nil {
				level = p.Level
			}
			if g := r.Permissions.Group; g != nil && g.Level > level {
				level = g.Level
			}
			out = append(out, Repo{Name: r.Path, DefaultBranch: r.DefaultBranch, Description: r.Description, Archived: r.Archived,
				Fork: len(r.ForkedFrom) > 0 && string(r.ForkedFrom) != "null", Private: r.Visibility != "public", PushedAt: r.LastActivityAt,
				Permission: gitlabPermission(level)})
		}
		return out, nil
	case Forgejo:
		var in []struct {
			Repo
			UpdatedAt   string       `json:"updated_at"`
			Permissions *permissions `json:"permissions"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return bad(err)
		}
		out := make([]Repo, 0, len(in))
		for _, r := range in {
			r.PushedAt = r.UpdatedAt
			r.Permission = r.Permissions.level()
			out = append(out, r.Repo)
		}
		return out, nil
	}
	var in []struct {
		Repo
		Permissions *permissions `json:"permissions"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return bad(err)
	}
	out := make([]Repo, 0, len(in))
	for _, r := range in {
		r.Permission = r.Permissions.level()
		out = append(out, r.Repo)
	}
	return out, nil
}

// permissions is the permissions object of GitHub and Forgejo; it is only
// present when the request carried a token.
type permissions struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Triage   bool `json:"triage"`
	Pull     bool `json:"pull"`
}

func (p *permissions) level() string {
	switch {
	case p == nil:
		return ""
	case p.Admin || p.Maintain:
		return PermAdmin
	case p.Push:
		return PermWrite
	case p.Pull || p.Triage:
		return PermRead
	}
	return ""
}

// gitlabPermission maps GitLab access levels: 40 maintainer, 30 developer,
// 10 guest and 20 reporter.
func gitlabPermission(level int) string {
	switch {
	case level >= 40:
		return PermAdmin
	case level >= 30:
		return PermWrite
	case level >= 10:
		return PermRead
	}
	return ""
}

// RepoExists reports whether owner/name still exists as far as the token can
// see: false only on a clean 404. Any other failure is an error, never "gone".
func (c Client) RepoExists(ctx context.Context, owner, name string) (bool, error) {
	if !ValidOwner(c.kind(), owner) || name == "" || strings.ContainsAny(name, "/?#%\\ ") || strings.HasPrefix(name, ".") {
		return false, fmt.Errorf("invalid repository %q/%q", owner, name)
	}
	path := "/repos/" + owner + "/" + name
	if c.kind() == GitLab {
		path = "/projects/" + owner + "%2F" + name
	}
	_, err := c.get(ctx, c.apiBase()+path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	}
	return false, err
}
