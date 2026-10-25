package api

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

const (
	aiStatusTTL     = 10 * time.Second
	aiStatusTimeout = 3 * time.Second
)

// ProviderStatus says whether a provider's endpoint answers right now. Cloud
// providers are not probed: a check would send traffic to a third party on
// every page view, so they report Checked=false.
type ProviderStatus struct {
	Name           string `json:"name"`
	Checked        bool   `json:"checked"`
	Online         bool   `json:"online"`
	ModelAvailable bool   `json:"model_available"`
	LatencyMS      int64  `json:"latency_ms,omitempty"`
	Error          string `json:"error,omitempty"`
}

// aiStatusCache keeps the last probe for a few seconds, so many open
// dashboards cost the model server one model-list call per provider.
type aiStatusCache struct {
	mu   sync.Mutex
	at   time.Time
	data []ProviderStatus
}

// handleAIStatus lists each local provider's model endpoint (no completion, so
// no tokens are spent) and reports whether it answered and serves the
// configured model.
func (s *Server) handleAIStatus(w http.ResponseWriter, r *http.Request) {
	out := []ProviderStatus{}
	if s.cfg.LLM != nil {
		out = s.aiStatus(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (s *Server) aiStatus(ctx context.Context) []ProviderStatus {
	c := &s.aiProbe
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data != nil && time.Since(c.at) < aiStatusTTL {
		return c.data
	}
	cfgs := s.cfg.LLM.Configs()
	out := make([]ProviderStatus, len(cfgs))
	var wg sync.WaitGroup
	for i, p := range cfgs {
		out[i] = ProviderStatus{Name: p.Name}
		if !p.Local() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = probeProvider(context.WithoutCancel(ctx), s.cfg.LLM, p)
		}()
	}
	wg.Wait()
	c.at, c.data = time.Now(), out
	return out
}

func probeProvider(ctx context.Context, reg *llm.Registry, p llm.ProviderConfig) ProviderStatus {
	st := ProviderStatus{Name: p.Name, Checked: true}
	ctx, cancel := context.WithTimeout(ctx, aiStatusTimeout)
	defer cancel()
	start := time.Now()
	models, err := reg.Models(ctx, p.Name)
	st.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		// The error can carry the endpoint's response; keep the UI line short.
		st.Error = "endpoint does not answer"
		return st
	}
	st.Online = true
	st.ModelAvailable = p.Model == "" || slices.Contains(models, p.Model) || slices.Contains(models, p.Model+":latest")
	return st
}
