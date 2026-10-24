package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

func (s *Server) handleHookActivity(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	id, err := execution.Require(r.Context())
	if err != nil {
		s.problem(w, 403, err)
		return
	}
	var in agentcfg.HookActivity
	// Unlike general API input, activity must reject unknown fields so clients
	// cannot quietly send source or prompts that we never intended to retain.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		s.problem(w, 400, errors.New("invalid hook activity"))
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		s.problem(w, 400, errors.New("invalid hook activity"))
		return
	}
	if err := in.Validate(); err != nil {
		s.problem(w, 400, err)
		return
	}
	h := sha256.Sum256([]byte("hook:" + id.TenantID + ":" + id.SessionID))
	now := time.Now()
	allowed, err := s.cfg.Accounts.St.TakeAttempt(r.Context(), hex.EncodeToString(h[:]), 600, now.Add(-time.Minute), now)
	if err != nil {
		s.problem(w, 503, errors.New("activity admission unavailable"))
		return
	}
	if !allowed {
		s.problem(w, 429, errors.New("session activity rate exceeded"))
		return
	}
	e, err := s.cfg.Accounts.RecordHookActivity(r.Context(), p, id, in)
	if err != nil {
		s.accountError(w, err)
		return
	}
	ctx := telemetry.WithTool(telemetry.WithModel(r.Context(), "none", "none"), "none")
	telemetry.AgentHookEvents.IncContext(ctx, in.Client, in.Event, in.Category, in.Outcome)
	if in.DurationMS != nil {
		telemetry.AgentHookDuration.ObserveContext(ctx, float64(*in.DurationMS)/1000, in.Client, in.Event)
	}
	for kind, count := range map[string]int64{"tool_input": in.InputBytes, "tool_output": in.OutputBytes, "prompt": in.PromptBytes, "text": in.TextBytes, "file_content": in.ContentBytes} {
		if count > 0 {
			telemetry.AgentHookBytes.AddContext(ctx, float64(count), in.Client, in.Event, kind)
		}
	}
	_, span := telemetry.Tracer().Start(ctx, "agent.hook", trace.WithAttributes(
		attribute.String("agent.client", in.Client), attribute.String("agent.event", in.Event),
		attribute.String("agent.tool_category", in.Category), attribute.String("agent.outcome", in.Outcome),
		attribute.String("execution.session_id", id.SessionID), attribute.Int64("agent.activity_id", e.ID)))
	span.SetAttributes(attribute.Int64("agent.input_bytes", in.InputBytes), attribute.Int64("agent.output_bytes", in.OutputBytes), attribute.Int64("agent.prompt_bytes", in.PromptBytes), attribute.Int64("agent.text_bytes", in.TextBytes))
	if in.DurationMS != nil {
		span.SetAttributes(attribute.Int64("agent.duration_ms", *in.DurationMS))
	}
	span.SetAttributes(attribute.Int64("agent.content_bytes", in.ContentBytes), attribute.Int64("agent.loop_count", in.LoopCount))
	for name, value := range map[string]*int64{
		"agent.context_tokens": in.ContextTokens, "agent.context_limit": in.ContextLimit,
		"agent.input_tokens": in.InputTokens, "agent.output_tokens": in.OutputTokens, "agent.cache_tokens": in.CacheTokens,
		"agent.message_count": in.MessageCount, "agent.tool_call_count": in.ToolCallCount, "agent.messages_to_compact": in.MessagesToCompact,
	} {
		if value != nil {
			span.SetAttributes(attribute.Int64(name, *value))
		}
	}
	for name, value := range map[string]*bool{"agent.sandbox": in.Sandbox, "agent.background": in.Background, "agent.first_compaction": in.FirstCompaction} {
		if value != nil {
			span.SetAttributes(attribute.Bool(name, *value))
		}
	}
	for name, value := range map[string]string{"agent.source": in.Source, "agent.failure_type": in.FailureType, "agent.permission_mode": in.PermissionMode, "agent.codesynapse_tool": in.Tool} {
		if value != "" {
			span.SetAttributes(attribute.String(name, value))
		}
	}
	if in.ContextPercent != nil {
		span.SetAttributes(attribute.Float64("agent.context_usage_percent", *in.ContextPercent))
	}
	span.End()
	writeJSON(w, http.StatusCreated, map[string]any{"id": e.ID, "at": e.At})
}

// Only the original participant may read session activity, including after a
// session closes. No repository content is included and membership is current.
func (s *Server) handleSessionActivity(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	m, err := scopeFrom(r.Context()).selected()
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	record, err := s.cfg.Accounts.St.ExecutionSessionByID(r.Context(), r.PathValue("session"))
	if err != nil || record.OrgID != m.Org.ID || record.UserID != p.User.ID {
		s.problem(w, 404, errors.New("session not found"))
		return
	}
	after, err := strconv.ParseInt(cmpOr(r.URL.Query().Get("after_id"), "0"), 10, 64)
	if err != nil || after < 0 {
		s.problem(w, 400, errors.New("invalid activity cursor"))
		return
	}
	rows, err := s.cfg.Accounts.St.ListEvents(r.Context(), accounts.EventFilter{OrgID: m.Org.ID, Actor: p.User.ID, Session: record.ID, Action: "agent.hook.", AfterID: after, Ascending: true, Limit: clamp(atoi(r.URL.Query().Get("limit")), 1, 1000, 100)})
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	type item struct {
		ID int64     `json:"id"`
		At time.Time `json:"at"`
		agentcfg.HookActivity
	}
	items := make([]item, 0, len(rows))
	for _, e := range rows {
		if e.MCP == nil || e.MCP.Method != "agent/hook" {
			continue
		}
		a := agentcfg.HookActivity{Client: e.MCP.Client, Event: e.MCP.Name}
		if json.Unmarshal([]byte(e.Detail), &a.HookMeasurements) != nil {
			continue
		}
		items = append(items, item{ID: e.ID, At: e.At, HookActivity: a})
	}
	writeJSON(w, 200, map[string]any{"events": items})
}
