package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Finding triage (docs/features/security.md): an organisation records that
// a finding is an accepted risk, a false positive or fixed by a commit or
// pull request, with who, when and why. Decisions live in the accounts
// database per organisation and repository entry; the shared graph keeps
// every finding. Reading needs viewer access, deciding repository admin
// access from a signed-in session or an API key; every route needs a
// registered runtime session. The service audits each change in the same
// transaction.

func (s *Server) triageRoutes() {
	s.route("GET /api/v1/repos/{repo}/triage", repoRole(accounts.Viewer), s.withExecution(s.handleListTriage))
	s.route("PUT /api/v1/repos/{repo}/triage", repoRole(accounts.Admin), s.withExecution(s.handleSetTriage))
	s.route("DELETE /api/v1/repos/{repo}/triage/{fingerprint}", repoRole(accounts.Admin), s.withExecution(s.handleClearTriage))
}

// maxFindingIDBytes bounds the finding ID a request names.
const maxFindingIDBytes = 512

type triageRequest struct {
	FindingID string     `json:"finding_id"`
	State     string     `json:"state"`
	Reason    string     `json:"reason"`
	Reference string     `json:"reference,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (s *Server) handleListTriage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	repo := r.PathValue("repo")
	org, err := policyOrg(r.Context(), repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	rows, err := s.cfg.Accounts.St.ListFindingTriage(r.Context(), org, repo, accounts.MaxFindingTriage)
	if err != nil {
		s.accountError(w, err)
		return
	}
	// The dashboard shows the triage action only to callers who may use it;
	// the PUT and DELETE routes check again.
	canEdit := false
	if p.Via == "session" || p.Via == "key" {
		_, err := s.cfg.Accounts.ResolveRepository(r.Context(), p, org, repo, accounts.Admin)
		canEdit = err == nil
	}
	writeJSON(w, 200, map[string]any{"triage": rows, "states": accounts.TriageStates, "can_edit": canEdit})
}

func (s *Server) handleSetTriage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var in triageRequest
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, 400, err)
		return
	}
	if in.FindingID == "" || len(in.FindingID) > maxFindingIDBytes {
		s.problem(w, 400, errors.New("finding_id names the finding to triage"))
		return
	}
	repo := r.PathValue("repo")
	org, err := policyOrg(r.Context(), repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	finding, fingerprint, err := s.triageFinding(r.Context(), repo, in.FindingID)
	if errors.Is(err, store.ErrNotFound) {
		s.problem(w, 404, fmt.Errorf("finding %w in this repository", store.ErrNotFound))
		return
	}
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	t := accounts.FindingTriage{OrgID: org, Graph: repo, Fingerprint: fingerprint, Rule: finding.Rule, File: finding.File,
		State: in.State, Reference: in.Reference, Reason: in.Reason, ExpiresAt: in.ExpiresAt}
	saved, err := s.cfg.Accounts.SetFindingTriage(r.Context(), p, t)
	if errors.Is(err, accounts.ErrAccessLimit) {
		s.problem(w, http.StatusConflict, fmt.Errorf("this repository has %d triage decisions; clear some first", accounts.MaxFindingTriage))
		return
	}
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, saved)
}

// triageFinding reads the finding the request names from the repository's
// graph and returns it with its fingerprint, so a decision always belongs
// to a finding that exists and its key is computed here, not by the client.
func (s *Server) triageFinding(ctx context.Context, repo, id string) (model.Node, string, error) {
	reader := store.ForRepository(s.st, repo)
	n, err := reader.Node(ctx, id)
	if err != nil {
		return model.Node{}, "", err
	}
	if n.Kind != model.KindFinding || n.Repo != repo {
		return model.Node{}, "", store.ErrNotFound
	}
	key, err := security.FindingKey(ctx, reader, n)
	return n, key, err
}

func (s *Server) handleClearTriage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	repo := r.PathValue("repo")
	org, err := policyOrg(r.Context(), repo)
	if err == nil {
		err = s.cfg.Accounts.ClearFindingTriage(r.Context(), p, org, repo, r.PathValue("fingerprint"))
	}
	if err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// triageOptions adds the organisation's active triage decisions to opts.
// When they cannot be read the findings stay active.
func (s *Server) triageOptions(ctx context.Context, org, graph string, opts security.Options) security.Options {
	rows, err := s.cfg.Accounts.St.ListFindingTriage(ctx, org, graph, accounts.MaxFindingTriage)
	if err != nil {
		return opts
	}
	return security.WithTriage(opts, rows, time.Now())
}
