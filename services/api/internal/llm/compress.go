package llm

import (
	"context"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// compressSystem is constant on purpose: it is the prompt prefix of the local
// model, and nothing per request may enter it.
const compressSystem = `You shorten text for another language model. Rewrite the text in terse telegraphic style: drop articles, filler, pleasantries and hedging; keep short sentences or fragments. Keep every number, identifier, file path, version, name and backtick-quoted span exactly as written. Do not add facts, opinions or explanations. Do not translate. Answer with the rewritten text only.`

// terseSuffix is appended to the system prompt of every compressed call so the
// cloud model answers briefly. It is constant, so a provider's prompt cache
// still matches the prefix.
const terseSuffix = "\n\nAnswer tersely: no preamble, no restating the question, no filler. Keep JSON keys, enum values, file paths and code exact."

const (
	// compressMinBytes: shorter paragraphs are not worth a model call.
	compressMinBytes = 400
	// compressMaxParagraphs bounds local calls per request.
	compressMaxParagraphs = 8
	// compressGain: an answer must be at least this much shorter to be used.
	compressGain = 0.9
)

var (
	backtickSpan = regexp.MustCompile("`[^`\n]+`")
	numberToken  = regexp.MustCompile(`[0-9][0-9.]*`)
)

// compressible reports whether a paragraph is prose. Code, diffs, JSON,
// tables and lists are left to the cloud model byte for byte.
func compressible(p string) bool {
	if len(p) < compressMinBytes || strings.Contains(p, "```") {
		return false
	}
	for _, line := range strings.Split(p, "\n") {
		t := strings.TrimLeft(line, " \t")
		if t == "" {
			continue
		}
		switch t[0] {
		case '+', '-', '@', '{', '[', '}', ']', '|', '#', '<', '>', '*':
			return false
		}
		if strings.HasPrefix(t, "diff ") || strings.HasPrefix(t, "index ") {
			return false
		}
	}
	return true
}

// keepsFacts checks that the rewrite still holds every number and quoted span
// of the original. A small model that drops one is not trusted.
func keepsFacts(orig, short string) bool {
	for _, tok := range backtickSpan.FindAllString(orig, -1) {
		if !strings.Contains(short, tok) {
			return false
		}
	}
	for _, tok := range numberToken.FindAllString(orig, -1) {
		if !strings.Contains(short, tok) {
			return false
		}
	}
	return true
}

// Compression states, as CompressionStatus reports them.
const (
	CompressionOn = "on"
	// CompressionDisabled: compress_prompts is false.
	CompressionDisabled = "disabled"
	// CompressionNoLocalModel: no local model is configured for the
	// "compress" task, so nothing is shortened.
	CompressionNoLocalModel = "no_local_model"
)

// CompressionStatus says whether prompts bound for cloud providers are
// shortened, and why not when they are not.
func (r *Registry) CompressionStatus() string {
	_, status := r.compressor()
	return status
}

// compressor resolves the provider for the "compress" task. The task is in
// LocalTasks, so ProviderFor never returns a cloud model for it; the Local
// check repeats that so the default can never send prose to the cloud for
// shortening.
func (r *Registry) compressor() (string, string) {
	r.mu.Lock()
	setting := r.cfg.CompressPrompts
	r.mu.Unlock()
	if setting != nil && !*setting {
		return "", CompressionDisabled
	}
	name, err := r.ProviderFor("compress", "")
	if err != nil {
		return "", CompressionNoLocalModel
	}
	if cfg, ok := r.Config(name); !ok || !cfg.Local() {
		return "", CompressionNoLocalModel
	}
	return name, CompressionOn
}

// compressCloud shortens the user part of a request that goes to a non-local
// provider, using the local "compress" task, and returns the request to send
// plus the bytes saved. Any problem (no local model, error, rewrite that lost
// a fact or did not shrink) keeps the original text.
func (r *Registry) compressCloud(ctx context.Context, name string, req Request) (Request, int) {
	if taskOf(ctx) == "compress" {
		return req, 0
	}
	if cfg, ok := r.Config(name); !ok || cfg.Local() {
		return req, 0
	}
	local, status := r.compressor()
	if status != CompressionOn {
		return req, 0
	}
	req.System += terseSuffix
	paras := strings.Split(req.User, "\n\n")
	saved, calls := 0, 0
	for i, p := range paras {
		if calls == compressMaxParagraphs {
			break
		}
		if !compressible(p) {
			continue
		}
		calls++
		short, ok := r.shorten(ctx, local, p)
		if !ok {
			continue
		}
		saved += len(p) - len(short)
		paras[i] = short
	}
	if saved > 0 {
		req.User = strings.Join(paras, "\n\n")
	}
	return req, saved
}

// shorten asks the local model to rewrite one paragraph. ok is false, and the
// original must be used, when the call failed or the rewrite is empty, did not
// shrink enough or lost a number or backticked name.
func (r *Registry) shorten(ctx context.Context, local, p string) (string, bool) {
	resp, err := r.Complete(WithTask(ctx, "compress"), local, Request{System: compressSystem, User: p})
	if err != nil {
		logx.FromContext(ctx).Debug("prompt compression skipped", "error", err)
		return "", false
	}
	short := strings.TrimSpace(resp.Text)
	if short == "" || float64(len(short)) > compressGain*float64(len(p)) || !keepsFacts(p, short) {
		return "", false
	}
	return short, true
}

// ShortenProse has the local "compress" model shorten one block of prose a
// person typed, for the agent gateway's cloud leg. The block is taken whole
// or not at all: it must pass compressible (no code, diffs, lists, JSON or
// tags) and hold nothing redact.Free would change, so a rewrite never puts a
// placeholder where a secret or personal detail was. ok is false, and the
// caller sends the original, when shortening is off, no local model is
// routed to "compress", or the rewrite fails the checks of shorten.
func (r *Registry) ShortenProse(ctx context.Context, text string) (string, bool) {
	if !compressible(text) || redact.Free(text) != text {
		return "", false
	}
	local, status := r.compressor()
	if status != CompressionOn {
		return "", false
	}
	return r.shorten(ctx, local, text)
}

// Prompt bytes the gateway removed for a cloud model the configuration does
// not name are counted under this provider and model: the requested model is
// client text and never a metric label. Statistics prices them at the
// reference model.
const (
	GatewayProvider = "gateway-cloud"
	GatewayModel    = "anthropic"
)

func init() { telemetry.AllowModels(GatewayProvider, GatewayModel) }

// RecordGatewayCompressed counts prompt bytes the gateway removed before a
// request went to the cloud. They are attributed to the configured cloud
// Anthropic provider whose model is the one requested, so Statistics prices
// them at its input price, and otherwise to GatewayProvider. model is only
// compared, never recorded.
func (r *Registry) RecordGatewayCompressed(ctx context.Context, model string, n int) {
	if n <= 0 {
		return
	}
	provider, label := GatewayProvider, GatewayModel
	r.mu.Lock()
	for _, p := range r.cfg.Providers {
		if p.Kind == KindAnthropic && !p.Local() && p.Model != "" && p.Model == model {
			provider, label = p.Name, p.Model
			if u := r.usage[p.Name]; u != nil {
				u.CompressedBytes += n
			}
			break
		}
	}
	r.mu.Unlock()
	ctx = telemetry.WithModel(ctx, provider, label)
	telemetry.LLMCompressed.AddContext(ctx, float64(n), telemetry.Tenant(ctx), provider, label)
}
