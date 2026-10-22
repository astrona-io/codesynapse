package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/memory/extract"
)

// OffloadClient hands conversation turns to the API, which extracts memory
// triples with a local model in the background. The MCP service has no
// model client of its own.
type OffloadClient struct {
	BaseURL string // e.g. http://api:8080
	Token   string // the API's bearer token, from the environment
	HTTP    *http.Client
}

// offloadJob is the API's answer (offload.Job), kept to what agents need.
type offloadJob struct {
	ID    string `json:"id"`
	Repo  string `json:"repo"`
	State string `json:"state"`
	Turns int    `json:"turns"`
}

func (c *OffloadClient) submit(ctx context.Context, repo, source string, turns []extract.Turn) (offloadJob, error) {
	body, err := json.Marshal(map[string]any{"turns": turns, "source": source})
	if err != nil {
		return offloadJob{}, err
	}
	u := strings.TrimRight(c.BaseURL, "/") + "/api/v1/repos/" + url.PathEscape(repo) + "/memory/offload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return offloadJob{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return offloadJob{}, fmt.Errorf("reaching the CodeSynapse API: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck // read-only response
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return offloadJob{}, err
	}
	if res.StatusCode != http.StatusAccepted {
		var p struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(data, &p) == nil && p.Detail != "" {
			return offloadJob{}, errors.New(p.Detail)
		}
		return offloadJob{}, fmt.Errorf("CodeSynapse API: status %d", res.StatusCode)
	}
	var j offloadJob
	return j, json.Unmarshal(data, &j)
}

func registerOffloadTool(s *mcp.Server, h *handlers, c *OffloadClient) {
	h.offload = c
	notDestructive := false
	write := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &notDestructive}
	mcp.AddTool(s, &mcp.Tool{Name: "offload_turns", Annotations: write, Description: "Hand older conversation turns to CodeSynapse, which extracts durable facts (decisions, preferences, ownership, open issues) with a local model in the background and saves them to this repository's memory. Returns at once with a job id; nothing is answered or blocked. Keep your last 5-10 exchanges in context and offload the ones before. Fails clearly when no local model is configured; then save triples yourself with upsert_graph_triples."}, h.offloadTurns)
}

type offloadTurnsIn struct {
	RepoArg
	Turns []extract.Turn `json:"turns" jsonschema:"older turns to offload, oldest first: role (user, assistant or tool) and text; at most 60"`
}

func (h *handlers) offloadTurns(ctx context.Context, req *mcp.CallToolRequest, in offloadTurnsIn) (*mcp.CallToolResult, offloadJob, error) {
	repo, err := h.indexedRepo(ctx, in.Repo)
	if err != nil {
		return nil, offloadJob{}, err
	}
	if len(in.Turns) == 0 || len(in.Turns) > extract.MaxTurns {
		return nil, offloadJob{}, fmt.Errorf("turns: give 1 to %d", extract.MaxTurns)
	}
	j, err := h.offload.submit(ctx, repo, clientName(req), in.Turns)
	return nil, j, err
}
