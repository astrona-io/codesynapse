package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/catalog"
	"github.com/parisnakitakejser/codesynapse/internal/curation"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// The architecture catalog: records kept as files in the repository, read
// through the same internal/catalog service the MCP tools use, on a reader
// fenced to the repository the route was authorised for.

func (s *Server) catalogOf(ctx context.Context, repo string) catalog.Service {
	return catalog.Service{R: store.ForRepository(s.st, repo), Items: s.st, Org: orgOfRepo(ctx, repo)}
}

// orgOfRepo is the organisation whose proposals and links the caller sees on
// repo: the one that gave the caller access. Outside accounts mode it is "".
// In accounts mode a caller with no organisation for the repository sees none.
func orgOfRepo(ctx context.Context, repo string) string {
	sc := scopeFrom(ctx)
	if sc.all {
		return ""
	}
	reach, ok := sc.reach[repo]
	if !ok || reach.Ambiguous {
		return "\x00none"
	}
	return reach.OrgID
}

func actorOf(ctx context.Context, repo string) catalog.Actor {
	a := catalog.Actor{Org: orgOfRepo(ctx, repo), Client: "web"}
	if p, ok := principal(ctx); ok {
		a.User = p.User.ID
	}
	return a
}

func (s *Server) catalogError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.problem(w, http.StatusNotFound, err)
	case errors.Is(err, curation.ErrInvalid):
		s.problem(w, http.StatusBadRequest, err)
	case errors.Is(err, catalog.ErrDisabled):
		s.problem(w, http.StatusServiceUnavailable, err)
	default:
		s.problem(w, 500, err)
	}
}

func commaList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// handleArchitecture lists the records: ?type= ?status= ?component= ?label=
// ?q= ?binding=1 ?limit= ?offset=.
func (s *Server) handleArchitecture(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := s.catalogOf(r.Context(), r.PathValue("repo")).List(r.Context(), r.PathValue("repo"), catalog.Filter{
		Type: q.Get("type"), Status: q.Get("status"), Component: q.Get("component"), Label: q.Get("label"), Q: q.Get("q"),
		BindingOnly: q.Get("binding") == "1", Limit: intParam(r, "limit", 50), Offset: intParam(r, "offset", 0),
	})
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, page)
}

// handleArchitectureRecord returns one record: ?id=ADR-0001.
func (s *Server) handleArchitectureRecord(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" || len(id) > 200 {
		s.problem(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}
	d, err := s.catalogOf(r.Context(), r.PathValue("repo")).Get(r.Context(), r.PathValue("repo"), id)
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, d)
}

// handleArchitectureIssues lists what a person should look at in the records.
func (s *Server) handleArchitectureIssues(w http.ResponseWriter, r *http.Request) {
	issues, src, err := s.catalogOf(r.Context(), r.PathValue("repo")).Issues(r.Context(), r.PathValue("repo"))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"issues": issues, "source": src})
}

// handleArchitectureContext finds the records that apply to a task:
// ?q= ?files=a,b ?symbols=x,y ?components=c ?limit=.
func (s *Server) handleArchitectureContext(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.catalogOf(r.Context(), r.PathValue("repo")).Context(r.Context(), r.PathValue("repo"), catalog.Task{
		Query: q.Get("q"), Files: commaList(q.Get("files")), Symbols: commaList(q.Get("symbols")), Components: commaList(q.Get("components")), Limit: intParam(r, "limit", 10),
	})
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

const maxCatalogBody = 64 << 10

func decodeBody(w http.ResponseWriter, r *http.Request, into any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCatalogBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return errors.Join(curation.ErrInvalid, err)
	}
	return nil
}

type proposalBody struct {
	Type       string   `json:"type"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Topic      string   `json:"topic"`
	Components []string `json:"components"`
	Labels     []string `json:"labels"`
	Supersedes []string `json:"supersedes"`
	Relates    []string `json:"relates"`
	Implements []string `json:"implements"`
}

// handleProposals lists the proposals.
func (s *Server) handleProposals(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	list, err := s.catalogOf(r.Context(), repo).Proposals(r.Context(), repo)
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"proposals": list})
}

// handleProposal returns one proposal with its text.
func (s *Server) handleProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	p, err := s.catalogOf(r.Context(), repo).Proposal(r.Context(), repo, r.PathValue("id"))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, p)
}

// handlePropose stores a draft record. It never approves anything: a proposal
// stays outside the repository's own records until it is committed as a file.
func (s *Server) handlePropose(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	var in proposalBody
	if err := decodeBody(w, r, &in); err != nil {
		s.catalogError(w, err)
		return
	}
	p, err := s.catalogOf(r.Context(), repo).Propose(r.Context(), repo, actorOf(r.Context(), repo), curation.ProposalInput(in))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// handleWithdrawProposal marks a proposal withdrawn (it stays as history).
func (s *Server) handleWithdrawProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	p, err := s.catalogOf(r.Context(), repo).Withdraw(r.Context(), repo, r.PathValue("id"))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, p)
}

// handleExportProposal serves the proposal as the Markdown file to commit.
func (s *Server) handleExportProposal(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	file, md, err := s.catalogOf(r.Context(), repo).Export(r.Context(), repo, r.PathValue("id"))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sbomFileNameBase(path.Base(file))+`"`)
	w.Header().Set("X-Suggested-Path", file)
	_, _ = w.Write([]byte(md)) //nolint:errcheck // client went away
}

type linkBody struct {
	Kind   string `json:"kind"`
	Record string `json:"record"`
	Target string `json:"target"`
	Note   string `json:"note"`
}

// handleLinks lists the links added through the catalog (?record= narrows them).
func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	list, err := s.catalogOf(r.Context(), repo).Links(r.Context(), repo, r.URL.Query().Get("record"))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"links": list})
}

// handleAddLink ties a record to code, to another record or to a component.
func (s *Server) handleAddLink(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	var in linkBody
	if err := decodeBody(w, r, &in); err != nil {
		s.catalogError(w, err)
		return
	}
	l, err := s.catalogOf(r.Context(), repo).AddLink(r.Context(), repo, actorOf(r.Context(), repo), in.Kind, in.Record, in.Target, in.Note)
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

// handleRemoveLink deletes a link added through the catalog.
func (s *Server) handleRemoveLink(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if err := s.catalogOf(r.Context(), repo).RemoveLink(r.Context(), repo, r.PathValue("id")); err != nil {
		s.catalogError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
