package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/services/api/internal/aitasks"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// ProviderInfo describes a configured AI provider for the UI. It never
// includes API keys.
type ProviderInfo struct {
	Name              string    `json:"name"`
	Kind              string    `json:"kind"`
	Model             string    `json:"model"`
	Endpoint          string    `json:"endpoint"`
	Local             bool      `json:"local"`
	HasKey            bool      `json:"has_key"`
	KeyEnv            string    `json:"key_env,omitempty"`
	SendsSource       bool      `json:"sends_source"`
	MaxOutput         int       `json:"max_output_tokens"`
	MaxInput          int       `json:"max_input_tokens"`
	InputBudgetPolicy string    `json:"input_budget_policy"`
	Context           int       `json:"context_window,omitempty"`
	Tasks             []string  `json:"tasks"`
	Usage             llm.Usage `json:"usage"`
}

// endpointHost shows scheme://host only, hiding paths and query strings.
func endpointHost(raw string) string {
	if raw == "" {
		return "api.anthropic.com"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (s *Server) handleAIProviders(w http.ResponseWriter, _ *http.Request) {
	out := []ProviderInfo{}
	routes := map[string][]string{}
	if s.cfg.LLM != nil {
		for task, name := range s.cfg.LLM.TaskRoutes() {
			routes[name] = append(routes[name], task)
		}
		for _, p := range s.cfg.LLM.Configs() {
			tasks := routes[p.Name]
			slices.Sort(tasks)
			out = append(out, ProviderInfo{
				Name: p.Name, Kind: p.Kind, Model: p.Model, Endpoint: endpointHost(p.BaseURL), Local: p.Local(),
				HasKey: p.HasKey(), KeyEnv: p.APIKeyEnv, SendsSource: p.AllowsSource(),
				MaxOutput: p.MaxOutputTokens, MaxInput: p.MaxInputTokens, InputBudgetPolicy: p.InputBudgetPolicy, Context: p.ContextWindow, Tasks: append([]string{}, tasks...),
				Usage: s.cfg.LLM.Usage(p.Name),
			})
		}
	}
	var routing llm.Routes
	if s.cfg.LLM != nil {
		routing = s.cfg.LLM.Routes()
	}
	writeJSON(w, 200, map[string]any{"providers": out, "tasks": llm.Tasks, "routing": routing})
}

func (s *Server) routesPath() string {
	if s.cfg.Workspace == "" {
		return ""
	}
	return filepath.Join(s.cfg.Workspace, ".codesynapse", "llm-routes.json")
}

// loadRoutes applies routing saved from the UI over the config file.
func (s *Server) loadRoutes() {
	p := s.routesPath()
	if p == "" || s.cfg.LLM == nil {
		return
	}
	data, err := os.ReadFile(p) //nolint:gosec // fixed path in the workspace
	if err != nil {
		return
	}
	var rt llm.Routes
	if json.Unmarshal(data, &rt) == nil {
		if err := s.cfg.LLM.SetRoutes(rt); err != nil {
			s.logger.Warn("saved AI routing ignored", "error", err)
		}
	}
}

// handlePutRouting sets the global model and per-task overrides.
func (s *Server) handlePutRouting(w http.ResponseWriter, r *http.Request) {
	if s.cfg.LLM == nil {
		s.problem(w, http.StatusConflict, errors.New("no AI providers configured"))
		return
	}
	var rt llm.Routes
	if err := decodeJSON(w, r, &rt); err != nil {
		s.problem(w, 400, err)
		return
	}
	if err := s.cfg.LLM.SetRoutes(rt); err != nil {
		s.problem(w, 400, err)
		return
	}
	if p := s.routesPath(); p != "" {
		data, _ := json.MarshalIndent(s.cfg.LLM.Routes(), "", "  ") //nolint:errcheck // plain struct
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			if err := os.WriteFile(p, data, 0o600); err != nil {
				s.logger.Warn("saving AI routing", "error", err)
			}
		}
	}
	writeJSON(w, 200, s.cfg.LLM.Routes())
}

// handleAITest checks a provider: list models, then a tiny completion.
func (s *Server) handleAITest(w http.ResponseWriter, r *http.Request) {
	if s.cfg.LLM == nil {
		s.problem(w, 404, errors.New("no AI providers configured"))
		return
	}
	name := r.PathValue("name")
	if _, ok := s.cfg.LLM.Config(name); !ok {
		s.problem(w, 404, errors.New("unknown provider"))
		return
	}
	out := map[string]any{"provider": name}
	models, err := s.cfg.LLM.Models(r.Context(), name)
	if err != nil {
		out["ok"], out["error"] = false, err.Error()
		writeJSON(w, 200, out)
		return
	}
	out["models"] = models
	if cfg, _ := s.cfg.LLM.Config(name); len(llm.MissingModels(cfg, models)) > 0 {
		out["missing"] = llm.MissingModels(cfg, models)
	}
	start := time.Now()
	resp, err := s.cfg.LLM.Complete(r.Context(), name, llm.Request{System: "Reply with the single word OK.", User: "Health check.", MaxTokens: 2048})
	out["seconds"] = time.Since(start).Round(100 * time.Millisecond).Seconds()
	if err != nil {
		out["ok"], out["error"] = false, err.Error()
	} else {
		out["ok"], out["reply"], out["model"] = true, resp.Text, resp.Model
	}
	writeJSON(w, 200, out)
}

type aiTaskRequest struct {
	Provider string `json:"provider"`
	Diff     string `json:"diff"`
}

// handleAITask queues an AI task; the job result is the review id.
func (s *Server) handleAITask(w http.ResponseWriter, r *http.Request) {
	repo, task := r.PathValue("repo"), r.PathValue("task")
	if !slices.Contains(llm.Tasks, task) || task == "fix" || task == "memory" || task == "embeddings" {
		s.problem(w, 400, errors.New("task must be one of security-triage, review, briefing, pr-summary, cleanup-briefing"))
		return
	}
	if s.cfg.LLM == nil {
		s.problem(w, http.StatusConflict, errors.New("no AI providers configured; add config/llm.yaml or CODESYNAPSE_LLM_BASE_URL"))
		return
	}
	var req aiTaskRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			s.problem(w, 400, err)
			return
		}
	}
	if _, err := s.cfg.LLM.ProviderFor(task, req.Provider); err != nil {
		s.problem(w, 400, err)
		return
	}
	if _, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo)); err != nil {
		s.problem(w, 500, err)
		return
	}
	if _, err := s.repoOrg(r, repo); err != nil {
		s.accountError(w, err)
		return
	}
	job, err := s.startAITask(r.Context(), repo, aiTaskParams{Task: task, Provider: req.Provider, Diff: []byte(req.Diff)}) //nolint:contextcheck // jobs outlive the request
	if err != nil {
		s.problem(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// aiTaskParams rebuild an AI task after a restart (jobs_durable.go).
type aiTaskParams struct {
	Task     string `json:"task"`
	Provider string `json:"provider,omitempty"`
	Diff     []byte `json:"diff,omitempty"`
}

// startAITask queues task for repo as the requester in ctx.
func (s *Server) startAITask(ctx context.Context, repo string, p aiTaskParams) (Job, error) {
	org, authorize := s.jobAuthorization(ctx, repo, accounts.Member)
	d, err := s.durableIf(ctx, "ai", accounts.Member, p)
	if err != nil {
		return Job{}, err
	}
	runner := aitasks.Runner{St: s.st, LLM: s.cfg.LLM, Authorize: authorize}
	in := aitasks.Input{Repo: repo, OrgID: org, Provider: p.Provider, Diff: p.Diff}
	return s.jobs.Start(repo, "", JobOptions{ //nolint:contextcheck // jobs outlive the request
		Kind: "ai", Task: p.Task, OrgID: org, Identity: identityOf(ctx), Authorize: authorize, Durable: d,
		Run: func(ctx context.Context) (string, error) { return runner.Run(ctx, p.Task, in) },
	})
}
