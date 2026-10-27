package llm

import (
	"context"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/logx"
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

// compressCloud shortens the user part of a request that goes to a non-local
// provider, using the local "compress" task, and returns the request to send
// plus the bytes saved. Any problem (no local model, error, rewrite that lost
// a fact or did not shrink) keeps the original text.
func (r *Registry) compressCloud(ctx context.Context, name string, req Request) (Request, int) {
	r.mu.Lock()
	on := r.cfg.CompressPrompts
	r.mu.Unlock()
	if !on || taskOf(ctx) == "compress" {
		return req, 0
	}
	if cfg, ok := r.Config(name); !ok || cfg.Local() {
		return req, 0
	}
	req.System += terseSuffix
	local, err := r.ProviderFor("compress", "")
	if err != nil {
		return req, 0
	}
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
		resp, err := r.Complete(WithTask(ctx, "compress"), local, Request{System: compressSystem, User: p})
		if err != nil {
			logx.FromContext(ctx).Debug("prompt compression skipped", "error", err)
			continue
		}
		short := strings.TrimSpace(resp.Text)
		if short == "" || float64(len(short)) > compressGain*float64(len(p)) || !keepsFacts(p, short) {
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
