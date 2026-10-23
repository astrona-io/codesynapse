package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store"
)

func (s *Server) repoPolicyRoutes() {
	s.ingestionRoutes()
	s.route("GET /api/v1/repos/{repo}/policy", repoRole(accounts.Viewer), s.handleRepositoryConfig)
	s.route("POST /api/v1/repos/{repo}/policy/preview", repoRole(accounts.Viewer), s.handlePreviewRepoPolicy)
	s.route("PUT /api/v1/repos/{repo}/policy", repoRole(accounts.Admin), s.handleApproveRepoPolicy)
	s.route("DELETE /api/v1/repos/{repo}/policy", repoRole(accounts.Admin), s.handleDeleteRepoPolicy)
	s.route("GET /api/v1/repos/{repo}/annotations", repoRole(accounts.Viewer), s.handleAnnotations)
	s.route("POST /api/v1/repos/{repo}/annotations", repoRole(accounts.Member), s.withExecution(s.handleAnnotate))
	s.route("GET /api/v1/repos/{repo}/annotations/jobs/{job}", repoRole(accounts.Viewer), s.withExecution(s.handleAnnotationJob))
	s.route("DELETE /api/v1/repos/{repo}/annotations/jobs/{job}", repoRole(accounts.Member), s.withExecution(s.handleCancelAnnotation))
}

func policyOrg(ctx context.Context, repo string) (string, error) {
	sc := scopeFrom(ctx)
	if reach, ok := sc.reach[repo]; ok {
		if reach.Ambiguous || reach.OrgID == "" {
			return "", accounts.ErrForbidden
		}
		return reach.OrgID, nil
	}
	m, err := sc.selected()
	return m.Org.ID, err
}

func (s *Server) handleRepositoryConfig(w http.ResponseWriter, r *http.Request) {
	if !s.accountsOn(w) {
		return
	}
	org, err := policyOrg(r.Context(), r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	var metadata ingest.PolicyMetadata
	n, err := s.st.Node(r.Context(), model.NodeID(r.PathValue("repo"), model.KindRepository, r.PathValue("repo")))
	if err == nil {
		metadata, err = ingest.RepositoryPolicy(n)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.problem(w, 500, err)
		return
	}
	approval, err := s.cfg.Accounts.St.GetRepoPolicy(r.Context(), org, r.PathValue("repo"))
	if err != nil && !errors.Is(err, accounts.ErrNotFound) {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"indexed": metadata, "approval": approval, "approval_active": policyActive(approval, metadata.Digest)})
}

type repoPolicyRequest struct {
	Config            string     `json:"config"`
	Digest            string     `json:"digest"`
	AllowExclusions   bool       `json:"allow_exclusions"`
	AllowSuppressions bool       `json:"allow_suppressions"`
	AllowAnnotation   bool       `json:"allow_annotation"`
	Reason            string     `json:"reason"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

func (s *Server) parseRepoPolicy(w http.ResponseWriter, r *http.Request) (repoPolicyRequest, *repoconfig.Policy, bool) {
	var in repoPolicyRequest
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, 400, err)
		return in, nil, false
	}
	p, err := repoconfig.Parse([]byte(in.Config))
	if err != nil {
		s.problem(w, 400, err)
		return in, nil, false
	}
	return in, p, true
}
func (s *Server) handlePreviewRepoPolicy(w http.ResponseWriter, r *http.Request) {
	_, p, ok := s.parseRepoPolicy(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"digest": p.Digest(), "config": p.Config(), "diagnostics": p.Diagnostics(), "requires_exclusion_approval": p.RequiresApproval()})
}
func (s *Server) handleApproveRepoPolicy(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.caller(w, r)
	if !ok {
		return
	}
	in, p, ok := s.parseRepoPolicy(w, r)
	if !ok {
		return
	}
	if in.Digest != p.Digest() {
		s.problem(w, 409, errors.New("configuration digest changed; preview and approve the exact configuration"))
		return
	}
	org, err := policyOrg(r.Context(), r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	data, err := json.Marshal(p.Config())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	approval := accounts.RepoPolicyApproval{OrgID: org, Graph: r.PathValue("repo"), Digest: p.Digest(), ConfigJSON: string(data), Reason: in.Reason, AllowExclusions: in.AllowExclusions, AllowSuppressions: in.AllowSuppressions, AllowAnnotation: in.AllowAnnotation, ExpiresAt: in.ExpiresAt}
	if err := s.cfg.Accounts.SetRepoPolicy(r.Context(), principal, approval); err != nil {
		s.accountError(w, err)
		return
	}
	s.handleRepositoryConfig(w, r)
}
func (s *Server) handleDeleteRepoPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	org, err := policyOrg(r.Context(), r.PathValue("repo"))
	if err == nil {
		err = s.cfg.Accounts.DeleteRepoPolicy(r.Context(), p, org, r.PathValue("repo"))
	}
	if err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func policyActive(a accounts.RepoPolicyApproval, digest string) bool {
	return digest != "" && a.Digest == digest && (a.ExpiresAt == nil || a.ExpiresAt.After(time.Now()))
}

// Shared graph shape requires the same reviewed configuration in every owner.
func (s *Server) approveIngestPolicy(ctx context.Context, repo string, p *repoconfig.Policy) (string, error) {
	if !p.RequiresApproval() {
		return "", nil
	}
	if s.cfg.Accounts == nil {
		return "", errors.New("repository exclusions require trusted accounts approval")
	}
	service := s.accountsFor(ctx)
	entries, err := service.St.GraphRepositories(ctx, repo)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", accounts.ErrForbidden
	}
	for _, entry := range entries {
		approval, err := service.St.GetRepoPolicy(ctx, entry.OrgID, repo)
		if err != nil || !policyActive(approval, p.Digest()) || !approval.AllowExclusions {
			return "", errors.New("repository exclusions require an active matching approval from every linked organisation")
		}
	}
	return p.Digest(), nil
}

// A missing, expired or changed approval keeps all detected findings active.
func (s *Server) repositorySecurityOptions(ctx context.Context, graph, minSeverity string) security.Options {
	opts := security.Options{MinSeverity: minSeverity}
	if s.cfg.Accounts == nil {
		return opts
	}
	org, err := policyOrg(ctx, graph)
	if err != nil {
		return opts
	}
	n, err := s.st.Node(ctx, model.NodeID(graph, model.KindRepository, graph))
	if err != nil {
		return opts
	}
	meta, err := ingest.RepositoryPolicy(n)
	if err != nil {
		return opts
	}
	approval, err := s.cfg.Accounts.St.GetRepoPolicy(ctx, org, graph)
	if err != nil || !policyActive(approval, meta.Digest) || !approval.AllowSuppressions {
		return opts
	}
	return security.ApprovedOptions(minSeverity, org, graph, meta.Digest, approval)
}
