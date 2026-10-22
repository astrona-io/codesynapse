package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/forge"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
)

const (
	// orgScanInterval is how often a watched organisation is listed again to
	// find new repositories.
	orgScanInterval = 5 * time.Minute
	// maxClonesPerScan bounds how many clones one scan queues; the rest wait
	// for the next scan. Jobs run one at a time anyway.
	maxClonesPerScan = 10
	// retryAfterFailure keeps a failing clone from being retried every scan.
	retryAfterFailure = time.Hour
)

// OrgRepo is one repository of a watched organisation, as last listed.
type OrgRepo struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Description   string `json:"description,omitempty"`
	Archived      bool   `json:"archived,omitempty"`
	Fork          bool   `json:"fork,omitempty"`
	Private       bool   `json:"private,omitempty"`
	PushedAt      string `json:"pushed_at,omitempty"`
	// LastTry and LastError record the last failed clone.
	LastTry   string `json:"last_try,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// Org is an organisation (or user) on a git host whose repositories are
// indexed. The same login on two hosts is two organisations.
type Org struct {
	// Provider is github, forgejo (also Gitea, Codeberg) or gitlab; Host is
	// where it lives, github.com by default. Both default for entries saved
	// before providers existed.
	Provider        forge.Kind `json:"provider"`
	Host            string     `json:"host"`
	Org             string     `json:"org"`
	IncludeArchived bool       `json:"include_archived"`
	IncludeForks    bool       `json:"include_forks"`
	// AutoIndex clones and indexes repositories as they appear.
	AutoIndex bool      `json:"auto_index"`
	LastScan  string    `json:"last_scan,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	NextScan  string    `json:"next_scan,omitempty"`
	Repos     []OrgRepo `json:"repos"`
}

// wants reports whether the org's filters include r.
func (o Org) wants(r OrgRepo) bool {
	return validRepoSegment(r.Name) && (o.IncludeArchived || !r.Archived) && (o.IncludeForks || !r.Fork)
}

// normalize fills in the defaults of entries saved before providers existed.
func (o *Org) normalize() {
	if o.Provider == "" {
		o.Provider = forge.GitHub
	}
	if o.Host == "" {
		o.Host = forge.DefaultHost
	}
	o.Host = strings.ToLower(o.Host)
}

// ident is the organisation's identity: its lower-case login on github.com,
// "host/login" elsewhere, so the same login on two hosts stays apart. It is
// the {org} of the HTTP routes.
func (o Org) ident() string {
	login := strings.ToLower(o.Org)
	if o.Host == "" || o.Host == forge.DefaultHost {
		return login
	}
	return o.Host + "/" + login
}

func (o Org) client() forge.Client { return forge.Client{Kind: o.Provider, Host: o.Host} }

// cloneURL is the remote of one repository of o.
func (o Org) cloneURL(repo string) string { return o.client().CloneURL(o.Org, repo) }

// fullName is the repository's identity in the workspace: "owner/name" on
// github.com, "host/owner/name" elsewhere, so repositories of the same name
// on two organisations or two providers never collide.
func (o Org) fullName(repo string) string { return forge.RepoName(o.Host, o.Org, repo) }

// indexedAs finds the indexed repository for r: by its full name, or by its
// remote for repositories indexed before names carried the owner (those keep
// their plain name until removed and added again).
func (o Org) indexedAs(r OrgRepo, byName map[string]model.Node) (model.Node, bool) {
	if n, ok := byName[o.fullName(r.Name)]; ok {
		return n, true
	}
	want := o.cloneURL(r.Name)
	if n, ok := byName[r.Name]; ok && sameRemote(n.URL, want) {
		return n, true
	}
	for _, n := range byName {
		if !strings.Contains(n.Name, "/") && n.URL != "" && sameRemote(n.URL, want) {
			return n, true
		}
	}
	return model.Node{}, false
}

// orgs persists watched organisations next to the auto-sync schedules.
type orgs struct {
	mu       sync.Mutex
	path     string
	all      map[string]*Org // key: Org.ident()
	scanning map[string]bool
}

func loadOrgs(dir string) (*orgs, error) {
	o := &orgs{all: map[string]*Org{}, scanning: map[string]bool{}}
	if dir == "" {
		return o, nil
	}
	o.path = filepath.Join(dir, ".codesynapse", "orgs.json")
	data, err := os.ReadFile(o.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return o, nil
	case err != nil:
		return nil, err
	}
	var list []*Org
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("reading %s: %w", o.path, err)
	}
	for _, org := range list {
		org.normalize()
		o.all[org.ident()] = org
	}
	return o, nil
}

// save writes atomically. Callers hold mu.
func (o *orgs) save() error {
	if o.path == "" {
		return nil
	}
	list := make([]*Org, 0, len(o.all))
	for _, org := range o.all {
		list = append(list, org)
	}
	slices.SortFunc(list, func(a, b *Org) int { return strings.Compare(a.ident(), b.ident()) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o700); err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}

func (o *orgs) get(login string) (Org, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	org, ok := o.all[strings.ToLower(login)]
	if !ok {
		return Org{}, false
	}
	return *org, true
}

func (o *orgs) list() []Org {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Org, 0, len(o.all))
	for _, org := range o.all {
		out = append(out, *org)
	}
	slices.SortFunc(out, func(a, b Org) int { return strings.Compare(a.ident(), b.ident()) })
	return out
}

func (o *orgs) put(org Org) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	org.normalize()
	o.all[org.ident()] = &org
	return o.save()
}

func (o *orgs) remove(login string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.all, strings.ToLower(login))
	return o.save()
}

// due returns organisations whose next scan has passed.
func (o *orgs) due(now time.Time) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for _, org := range o.all {
		next, err := time.Parse(time.RFC3339, org.NextScan)
		if err != nil || !now.Before(next) {
			out = append(out, org.ident())
		}
	}
	slices.Sort(out)
	return out
}

// begin marks a scan of login as running; false if one already is.
func (o *orgs) begin(login string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := strings.ToLower(login)
	if o.scanning[key] {
		return false
	}
	o.scanning[key] = true
	return true
}

func (o *orgs) end(login string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.scanning, strings.ToLower(login))
}

// forgeClient returns the API client of o's host. Hosts other than github.com
// come from the user, so they go through the same SSRF check as clone URLs
// (no internal addresses unless CODESYNAPSE_ALLOW_PRIVATE_GIT), and the token
// configured for a host is sent to that host only.
func (s *Server) forgeClient(ctx context.Context, o Org) (forge.Client, error) {
	c := o.client()
	c.BaseURL = s.cfg.ForgeAPI
	if s.cfg.ForgeAPI == "" && o.Host != forge.DefaultHost {
		if _, err := gitx.ValidateRemote(ctx, o.cloneURL("x"), gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit}); err != nil {
			return c, fmt.Errorf("%w: %w", errBadRequest, err)
		}
	}
	c.Token = s.authFor(o.cloneURL("x")).Token
	return c, nil
}

// scanOrg lists the organisation's repositories, stores the result and, when
// AutoIndex is on, queues clones for the ones that are not indexed yet.
func (s *Server) scanOrg(ctx context.Context, login string, now time.Time) (Org, error) {
	if !s.orgs.begin(login) {
		o, _ := s.orgs.get(login)
		return o, fmt.Errorf("%w: a scan of %s is already running", errConflict, login)
	}
	defer s.orgs.end(login)
	o, ok := s.orgs.get(login)
	if !ok {
		return Org{}, fmt.Errorf("%w: %s is not watched", errBadRequest, login)
	}
	return s.scan(ctx, o, now)
}

func (s *Server) scan(ctx context.Context, o Org, now time.Time) (Org, error) {
	client, err := s.forgeClient(ctx, o)
	var listed []forge.Repo
	if err == nil {
		listed, err = client.ListRepos(ctx, o.Org)
	}
	o.NextScan = now.Add(orgScanInterval).UTC().Format(time.RFC3339)
	if err != nil {
		// Keep the previous list; a provider hiccup must not forget repositories.
		o.LastError = err.Error()
		if perr := s.orgs.put(o); perr != nil {
			s.logger.Warn("saving organisation", "org", o.Org, "error", perr)
		}
		return o, err
	}
	previous := map[string]OrgRepo{}
	for _, r := range o.Repos {
		previous[r.Name] = r
	}
	o.Repos = o.Repos[:0:0]
	for _, r := range listed {
		or := OrgRepo{Name: r.Name, DefaultBranch: r.DefaultBranch, Description: r.Description,
			Archived: r.Archived, Fork: r.Fork, Private: r.Private, PushedAt: r.PushedAt}
		if p, ok := previous[r.Name]; ok {
			or.LastTry, or.LastError = p.LastTry, p.LastError
		}
		o.Repos = append(o.Repos, or)
	}
	o.LastScan, o.LastError = now.UTC().Format(time.RFC3339), ""
	if o.AutoIndex && s.cfg.Workspace != "" {
		s.indexMissing(ctx, &o, now)
	}
	return o, s.orgs.put(o)
}

// indexMissing queues clones for wanted repositories that are not indexed,
// running or recently failed.
func (s *Server) indexMissing(ctx context.Context, o *Org, now time.Time) {
	indexed := map[string]model.Node{}
	if repos, err := s.st.Repos(ctx); err == nil {
		for _, n := range repos {
			indexed[n.Name] = n
		}
	}
	latest := s.latestJobs()
	started := 0
	for i := range o.Repos {
		r := &o.Repos[i]
		if !o.wants(*r) {
			continue
		}
		if _, ok := o.indexedAs(*r, indexed); ok {
			r.LastTry, r.LastError = "", ""
			continue
		}
		if j, ok := latest[o.fullName(r.Name)]; ok {
			switch j.State {
			case JobRunning:
				continue
			case JobFailed:
				r.LastTry, r.LastError = j.FinishedAt, j.Error
			}
		}
		if t, err := time.Parse(time.RFC3339, r.LastTry); err == nil && r.LastError != "" && now.Sub(t) < retryAfterFailure {
			continue
		}
		if started >= maxClonesPerScan {
			break
		}
		if _, err := s.cloneOrgRepo(ctx, *o, r.Name); err != nil { //nolint:contextcheck // jobs outlive the scan
			if !errors.Is(err, errConflict) {
				r.LastTry, r.LastError = now.UTC().Format(time.RFC3339), err.Error()
			}
			continue
		}
		r.LastTry, r.LastError = now.UTC().Format(time.RFC3339), ""
		started++
	}
}

// cloneOrgRepo queues a clone of one repository of o. The URL is built from
// the host, the organisation and a name that came from the provider and
// passed validRepoSegment, never taken from the API answer.
func (s *Server) cloneOrgRepo(ctx context.Context, o Org, name string) (Job, error) {
	if !validRepoSegment(name) || !validRepoSegment(strings.ToLower(o.Org)) {
		return Job{}, fmt.Errorf("%w: unsupported repository name %q", errBadRequest, name)
	}
	remote, full := o.cloneURL(name), o.fullName(name)
	if o.Host != forge.DefaultHost && s.cfg.ForgeAPI == "" {
		var err error
		if remote, err = gitx.ValidateRemote(ctx, remote, gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit}); err != nil {
			return Job{}, fmt.Errorf("%w: %w", errBadRequest, err)
		}
	}
	if s.cloneHook != nil {
		return s.cloneHook(remote, full)
	}
	return s.startClone(remote, full, "") //nolint:contextcheck // jobs outlive the request
}

// latestJobs maps repository to its most recent job.
func (s *Server) latestJobs() map[string]Job {
	out := map[string]Job{}
	for _, j := range s.jobs.List() { // newest first
		if _, ok := out[j.Repo]; !ok && (j.Kind == "clone" || j.Kind == "sync" || j.Kind == "ingest") {
			out[j.Repo] = j
		}
	}
	return out
}

// scanDue starts a background scan for every organisation that is due.
func (s *Server) scanDue(ctx context.Context, now time.Time, logger *slog.Logger) {
	for _, login := range s.orgs.due(now) {
		go func() {
			if _, err := s.scanOrg(ctx, login, now); err != nil && !errors.Is(err, errConflict) {
				logger.Warn("organisation scan failed", "org", login, "error", err)
			}
		}()
	}
}

// --- HTTP -------------------------------------------------------------------

// OrgRepoView is an OrgRepo with its state in this workspace.
type OrgRepoView struct {
	OrgRepo
	// Repo is the name of the repository in this workspace (owner/name).
	Repo string `json:"repo"`
	// Status: indexed, indexing, failed, pending (queued for the next scan),
	// available (auto-index is off), conflict (another repository uses the
	// name) or excluded (filtered out).
	Status string `json:"status"`
}

// OrgView is an Org with per-repository status and counts.
type OrgView struct {
	Org
	// Key identifies the organisation in the API paths.
	Key    string         `json:"key"`
	Repos  []OrgRepoView  `json:"repos"`
	Counts map[string]int `json:"counts"`
}

func (s *Server) orgView(ctx context.Context, o Org) OrgView {
	indexed := map[string]model.Node{}
	if repos, err := s.st.Repos(ctx); err == nil {
		for _, n := range repos {
			indexed[n.Name] = n
		}
	}
	latest := s.latestJobs()
	v := OrgView{Org: o, Key: o.ident(), Repos: make([]OrgRepoView, 0, len(o.Repos)), Counts: map[string]int{}}
	for _, r := range o.Repos {
		rv := OrgRepoView{OrgRepo: r, Repo: o.fullName(r.Name)}
		n, isIndexed := o.indexedAs(r, indexed)
		if isIndexed {
			rv.Repo = n.Name
		}
		switch {
		case !o.wants(r):
			rv.Status = "excluded"
		case isIndexed && sameRemote(n.URL, o.cloneURL(r.Name)):
			rv.Status = "indexed"
		case isIndexed:
			rv.Status = "conflict"
		case latest[o.fullName(r.Name)].State == JobRunning:
			rv.Status = "indexing"
		case r.LastError != "":
			rv.Status = "failed"
		case o.AutoIndex:
			rv.Status = "pending"
		default:
			rv.Status = "available"
		}
		v.Counts[rv.Status]++
		v.Repos = append(v.Repos, rv)
	}
	v.Org.Repos = nil
	return v
}

func sameRemote(a, b string) bool {
	norm := func(s string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".git") }
	return norm(a) == norm(b)
}

func (s *Server) orgsEnabled(w http.ResponseWriter) bool {
	if s.cfg.Workspace == "" {
		s.problem(w, http.StatusNotImplemented, errors.New("no workspace configured (CODESYNAPSE_WORKSPACE)"))
		return false
	}
	return true
}

func (s *Server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	list := s.orgs.list()
	out := make([]OrgView, 0, len(list))
	for _, o := range list {
		out = append(out, s.orgView(r.Context(), o))
	}
	writeJSON(w, 200, map[string]any{"orgs": out, "enabled": s.cfg.Workspace != ""})
}

type addOrgRequest struct {
	// Provider is github, forgejo (gitea, codeberg) or gitlab; guessed for
	// github.com, gitlab.com and codeberg.org.
	Provider string `json:"provider"`
	// Host defaults to github.com.
	Host string `json:"host"`
	// Org is a login, "host/login", or a URL such as https://codeberg.org/team/*.
	Org             string `json:"org"`
	IncludeArchived bool   `json:"include_archived"`
	IncludeForks    bool   `json:"include_forks"`
	AutoIndex       *bool  `json:"auto_index"`
}

// parseOrgInput resolves the provider, host and login of an add request.
func parseOrgInput(provider, host, input string) (forge.Kind, string, string, error) {
	input = strings.TrimSpace(input)
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(input, "://") {
		u, err := url.Parse(input)
		if err != nil || u.Scheme != "https" || u.User != nil {
			return "", "", "", errors.New("use an https:// URL without credentials")
		}
		if u.Port() != "" {
			return "", "", "", errors.New("hosts with a port are not supported")
		}
		host, input = strings.ToLower(u.Hostname()), strings.TrimPrefix(u.Path, "/")
	}
	input = strings.TrimSuffix(strings.TrimSuffix(input, "/*"), "/")
	if first, rest, ok := strings.Cut(input, "/"); ok {
		switch {
		case host == "" && strings.Contains(first, "."):
			host, input = strings.ToLower(first), rest // "codeberg.org/team"
		default:
			input = first // "team/repo" or "team/*": only the organisation counts
		}
		if strings.Contains(input, "/") {
			return "", "", "", errors.New("give the organisation only, such as team or codeberg.org/team")
		}
	}
	if host == "" {
		host = forge.DefaultHost
	}
	if !forge.ValidHost(host) {
		return "", "", "", fmt.Errorf("%q is not a valid host name (no scheme, port or path)", host)
	}
	kind, ok := forge.ParseKind(provider)
	switch {
	case provider != "" && !ok:
		return "", "", "", errors.New("provider must be github, forgejo (also Gitea, Codeberg) or gitlab")
	case provider == "":
		if kind, ok = forge.KindForHost(host); !ok {
			return "", "", "", fmt.Errorf("choose the provider of %s: github, forgejo or gitlab", host)
		}
	}
	if !forge.ValidOwner(kind, input) {
		return "", "", "", fmt.Errorf("%q is not a valid %s organisation name", input, kind)
	}
	return kind, host, input, nil
}

// handleAddOrg starts watching an organisation: it lists the repositories
// right away and, unless auto_index is false, queues their clones.
func (s *Server) handleAddOrg(w http.ResponseWriter, r *http.Request) {
	if !s.orgsEnabled(w) {
		return
	}
	var req addOrgRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	kind, host, login, err := parseOrgInput(req.Provider, req.Host, req.Org)
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	o := Org{Provider: kind, Host: host, Org: login, IncludeArchived: req.IncludeArchived, IncludeForks: req.IncludeForks, AutoIndex: req.AutoIndex == nil || *req.AutoIndex}
	key := o.ident()
	if _, ok := s.orgs.get(key); ok {
		s.problem(w, http.StatusConflict, fmt.Errorf("%s is already watched", key))
		return
	}
	if !s.orgs.begin(key) {
		s.problem(w, http.StatusConflict, fmt.Errorf("a scan of %s is already running", key))
		return
	}
	defer s.orgs.end(key)
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	o, err = s.scan(ctx, o, time.Now())
	if err != nil {
		s.orgs.mu.Lock()
		delete(s.orgs.all, key) // never listed: do not keep it
		s.orgs.mu.Unlock()
		s.orgError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.orgView(r.Context(), o))
}

func (s *Server) orgError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, forge.ErrNotFound):
		s.problem(w, 404, err)
	case errors.Is(err, errBadRequest):
		s.problem(w, 400, err)
	case errors.Is(err, errConflict):
		s.problem(w, http.StatusConflict, err)
	default:
		s.problem(w, http.StatusBadGateway, err)
	}
}

type updateOrgRequest struct {
	IncludeArchived bool `json:"include_archived"`
	IncludeForks    bool `json:"include_forks"`
	AutoIndex       bool `json:"auto_index"`
}

func (s *Server) handleUpdateOrg(w http.ResponseWriter, r *http.Request) {
	o, ok := s.orgs.get(r.PathValue("org"))
	if !ok {
		s.problem(w, 404, errors.New("organisation is not watched"))
		return
	}
	var req updateOrgRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	o.IncludeArchived, o.IncludeForks, o.AutoIndex = req.IncludeArchived, req.IncludeForks, req.AutoIndex
	if err := s.orgs.put(o); err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, s.orgView(r.Context(), o))
}

func (s *Server) handleScanOrg(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	o, err := s.scanOrg(ctx, r.PathValue("org"), time.Now())
	if err != nil {
		s.orgError(w, err)
		return
	}
	writeJSON(w, 200, s.orgView(r.Context(), o))
}

// handleDeleteOrg stops watching an organisation. Repositories already
// indexed stay; remove them one by one.
func (s *Server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.orgs.get(r.PathValue("org")); !ok {
		s.problem(w, 404, errors.New("organisation is not watched"))
		return
	}
	if err := s.orgs.remove(r.PathValue("org")); err != nil {
		s.problem(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIndexOrgRepo clones and indexes one listed repository (the way to
// index repositories when auto-index is off).
func (s *Server) handleIndexOrgRepo(w http.ResponseWriter, r *http.Request) {
	o, ok := s.orgs.get(r.PathValue("org"))
	if !ok {
		s.problem(w, 404, errors.New("organisation is not watched"))
		return
	}
	name := r.PathValue("name")
	if !slices.ContainsFunc(o.Repos, func(x OrgRepo) bool { return x.Name == name }) {
		s.problem(w, 404, fmt.Errorf("%s/%s was not found in the last scan", o.ident(), name))
		return
	}
	job, err := s.cloneOrgRepo(r.Context(), o, name) //nolint:contextcheck // jobs outlive the request
	if err != nil {
		s.orgError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}
