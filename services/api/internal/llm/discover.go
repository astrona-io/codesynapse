package llm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Auto is the model (or embedding_model) value that lets CodeSynapse pick
// from what a local server serves: the strongest chat model, and a model
// for embeddings. It is resolved at start-up and again every few minutes,
// so a model pulled or removed later is picked up without a restart.
const Auto = "auto"

// ServedModel is one model a local server offers, with what it can do.
type ServedModel struct {
	Name      string  `json:"name"`
	Chat      bool    `json:"chat"`
	Embedding bool    `json:"embedding"`
	Thinking  bool    `json:"thinking,omitempty"`
	ParamsB   float64 `json:"params_b,omitempty"` // billions of parameters, 0 when unknown
	Context   int     `json:"context,omitempty"`  // context length in tokens, 0 when unknown
	Bytes     int64   `json:"bytes,omitempty"`
}

// maxServed bounds what one server's list may hold.
const maxServed = 100

var discoverClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Discover lists what the OpenAI-compatible server at baseURL serves. Ollama
// is asked through its own API, which says what each model can do, how large
// it is and its context length; any other server answers /models with names
// only, and embedding models are told apart by name.
func Discover(ctx context.Context, baseURL, apiKey string) ([]ServedModel, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if models, ok := discoverOllama(ctx, strings.TrimSuffix(baseURL, "/v1")); ok {
		return models, nil
	}
	var res struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, http.MethodGet, baseURL+"/models", apiKey, nil, &res); err != nil {
		return nil, err
	}
	out := make([]ServedModel, 0, len(res.Data))
	for _, m := range res.Data[:min(len(res.Data), maxServed)] {
		embed := strings.Contains(strings.ToLower(m.ID), "embed")
		out = append(out, ServedModel{Name: m.ID, Chat: !embed, Embedding: embed})
	}
	return out, nil
}

// discoverOllama answers ok=false when root is not an Ollama server.
func discoverOllama(ctx context.Context, root string) ([]ServedModel, bool) {
	var tags struct {
		Models []struct {
			Name         string   `json:"name"`
			Size         int64    `json:"size"`
			Capabilities []string `json:"capabilities"`
			Details      struct {
				ParameterSize string `json:"parameter_size"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := getJSON(ctx, http.MethodGet, root+"/api/tags", "", nil, &tags); err != nil || tags.Models == nil {
		return nil, false
	}
	out := make([]ServedModel, 0, len(tags.Models))
	for _, m := range tags.Models[:min(len(tags.Models), maxServed)] {
		s := ServedModel{Name: m.Name, Bytes: m.Size, ParamsB: parseParams(m.Details.ParameterSize)}
		caps := m.Capabilities
		var show struct {
			Capabilities []string       `json:"capabilities"`
			ModelInfo    map[string]any `json:"model_info"`
		}
		// The context length is only in /api/show; older servers also keep
		// the capabilities there.
		if getJSON(ctx, http.MethodPost, root+"/api/show", "", map[string]string{"model": m.Name}, &show) == nil {
			if len(caps) == 0 {
				caps = show.Capabilities
			}
			for k, v := range show.ModelInfo {
				switch n, _ := v.(float64); {
				case strings.HasSuffix(k, ".context_length"):
					s.Context = int(n)
				case k == "general.parameter_count" && n > 0:
					s.ParamsB = n / 1e9
				}
			}
		}
		s.Chat = slices.Contains(caps, "completion")
		s.Embedding = slices.Contains(caps, "embedding")
		s.Thinking = slices.Contains(caps, "thinking")
		if len(caps) == 0 {
			s.Embedding = strings.Contains(strings.ToLower(m.Name), "embed")
			s.Chat = !s.Embedding
		}
		out = append(out, s)
	}
	return out, true
}

var paramsRe = regexp.MustCompile(`^([0-9.]+)\s*([KMBT])$`)

// parseParams reads Ollama's parameter_size ("27.8B", "137M") as billions.
func parseParams(s string) float64 {
	m := paramsRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v * map[string]float64{"K": 1e-6, "M": 1e-3, "B": 1, "T": 1e3}[m[2]]
}

// Best picks the chat model and the embedding model to use: the chat model
// with the most parameters (then the largest file, then the name), since
// answer quality matters more than speed here, and the first embedding
// model by name. Either is "" when the server has none.
func Best(models []ServedModel) (chat ServedModel, embed ServedModel) {
	for _, m := range models {
		if m.Chat && !m.Embedding && better(m, chat) {
			chat = m
		}
		if m.Embedding && (embed.Name == "" || m.Name < embed.Name) {
			embed = m
		}
	}
	return chat, embed
}

func better(a, b ServedModel) bool {
	if b.Name == "" {
		return true
	}
	return cmp.Or(cmp.Compare(a.ParamsB, b.ParamsB), cmp.Compare(a.Bytes, b.Bytes), cmp.Compare(b.Name, a.Name)) > 0
}

func getJSON(ctx context.Context, method, url, apiKey string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	res, err := discoverClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // read-only
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %w", url, httpStatus(res.StatusCode))
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}

// httpStatus is a non-2xx answer; its text is the same as before callers
// could tell it apart from a network error.
type httpStatus int

func (h httpStatus) Error() string { return fmt.Sprintf("HTTP %d", int(h)) }
