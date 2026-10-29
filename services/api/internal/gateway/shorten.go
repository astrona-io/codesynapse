package gateway

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
)

// Prompt shortening on the cloud leg: the local model rewrites prose a person
// typed into a /v1/messages request before it goes to Anthropic. Only text
// blocks of user messages are touched; tool_use, tool_result, images, the
// system prompt, tagged blocks (<system-reminder> and the like) and code are
// passed on byte for byte. It is on unless the organisation turned it off,
// covers every cloud leg (also a request the local model was offered and did
// not answer; that model gets the text as typed) and never touches a model on
// an exclude list.
//
// Claude Code sends the whole conversation with every request, and the
// provider's prompt cache matches the bytes of that prefix. So a rewrite is
// made once, when its block first appears in the newest user message, and
// replayed from a per-session memo for every later request. A block without a
// memo entry (after a restart, or on another replica) goes out as typed: one
// cache miss, never a second, different rewrite of an earlier turn.
const (
	// ShortenTimeout bounds the one local call a request may make.
	ShortenTimeout = 8 * time.Second
	// shortenMinBytes: shorter prompts save too little to be worth a call.
	shortenMinBytes = 400
	// shortenMaxBytes: larger blocks are pasted material, not a typed prompt,
	// and would not be rewritten within the timeout.
	shortenMaxBytes = 16 << 10
	// The memo is bounded per session, in sessions, in bytes and in time. It
	// lives in memory only and is never persisted or logged.
	memoPerSession = 512
	memoSessions   = 256
	memoBytes      = 32 << 20
	memoTTL        = 24 * time.Hour
)

type shortenKey struct{}

// WithShortenPrompts records the organisation's shorten_prompts choice for
// the request.
func WithShortenPrompts(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, shortenKey{}, on)
}

// shortenOn is true unless the organisation turned shortening off.
func shortenOn(ctx context.Context) bool {
	on, chose := ctx.Value(shortenKey{}).(bool)
	return on || !chose
}

// Shortened is what Apply did to one request: sizes only, never text.
type Shortened struct {
	// Changed says the forwarded body differs from the one received.
	Changed bool
	// Removed is the bytes taken out of this request, new and replayed
	// rewrites together.
	Removed int
	// New is the bytes removed by a rewrite made for this request. It is
	// what Statistics counts: replays mostly hit the provider's prompt
	// cache, so counting them again would overstate the saving.
	New int
}

// Shortener rewrites the user's own prose on the cloud leg.
type Shortener struct {
	// Ready says whether a local model can shorten at all; when it cannot,
	// the body is not even parsed and goes out unchanged.
	Ready func() bool
	// Rewrite asks the local model for a shorter version of text; ok false
	// keeps the original. It must check facts itself (llm.ShortenProse).
	Rewrite func(ctx context.Context, text string) (string, bool)
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	memo memo
}

// Apply returns the body to forward. Any doubt (no session, a body it cannot
// read, a duplicate key, a failed or slow rewrite) keeps the original bytes.
func (s *Shortener) Apply(ctx context.Context, body []byte) ([]byte, Shortened) {
	if s == nil || s.Rewrite == nil || (s.Ready != nil && !s.Ready()) {
		return body, Shortened{}
	}
	id, ok := execution.From(ctx)
	if !ok || id.SessionID == "" {
		return body, Shortened{}
	}
	spans, err := userTexts(body)
	if err != nil || len(spans) == 0 {
		return body, Shortened{}
	}
	now := s.now()
	called := false
	type change struct {
		span  textSpan
		short string
	}
	var changes []change
	var res Shortened
	for _, sp := range spans {
		if !candidate(sp.text) {
			continue
		}
		key := sha256.Sum256([]byte(sp.text))
		e, found := s.memo.get(id.SessionID, key, now)
		if !found {
			// Only the newest user message may cost a local call, and
			// only one per request.
			if !sp.newest || called {
				continue
			}
			called = true
			e = s.rewrite(ctx, id.SessionID, key, sp.text, now)
			if e == nil {
				continue
			}
			if e.ok {
				res.New += len(sp.text) - len(e.short)
			}
		} else if !e.wait(ctx) {
			continue
		}
		if e.ok {
			changes = append(changes, change{sp, e.short})
			res.Removed += len(sp.text) - len(e.short)
		}
	}
	if len(changes) == 0 {
		return body, Shortened{}
	}
	out := make([]byte, 0, len(body))
	at := 0
	for _, c := range changes {
		lit, err := jsonString(c.short)
		if err != nil {
			return body, Shortened{}
		}
		out = append(out, body[at:c.span.start]...)
		out = append(out, lit...)
		at = c.span.end
	}
	out = append(out, body[at:]...)
	res.Changed = true
	return out, res
}

func (s *Shortener) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// rewrite makes the one local call and memoizes its outcome, a refusal
// included, so the block is sent the same way on every later turn. A request
// that ended (the agent went away) leaves no entry. nil means keep the text.
func (s *Shortener) rewrite(ctx context.Context, session string, key [32]byte, text string, now time.Time) *memoEntry {
	e, leader := s.memo.reserve(session, key, now)
	if !leader {
		if !e.wait(ctx) {
			return nil
		}
		return e
	}
	cctx, cancel := context.WithTimeout(ctx, ShortenTimeout)
	short, ok := s.Rewrite(cctx, text)
	cancel()
	if ok && (short == "" || len(short) >= len(text)) {
		ok = false
	}
	if ctx.Err() != nil {
		s.memo.drop(session, e)
		return nil
	}
	s.memo.finish(session, e, short, ok)
	return e
}

// candidate is the gateway's cheap filter; ShortenProse checks the rest
// (code, lists, JSON, secrets).
func candidate(text string) bool {
	if len(text) < shortenMinBytes || len(text) > shortenMaxBytes {
		return false
	}
	t := strings.TrimSpace(text)
	return t != "" && t[0] != '<' && !strings.Contains(t, "<system-reminder") && !strings.Contains(t, "```")
}

// jsonString encodes s as a JSON string the same way every time.
func jsonString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// memo holds rewrites per runtime session, keyed by the SHA-256 of the
// original block.
type memo struct {
	mu       sync.Mutex
	sessions map[string]*sessionMemo
	order    list.List // *sessionMemo, most recently used first
	bytes    int
}

type sessionMemo struct {
	id      string
	el      *list.Element
	entries map[[32]byte]*list.Element
	lru     list.List // *memoEntry, most recently used first
	bytes   int
	used    time.Time
}

type memoEntry struct {
	key   [32]byte
	at    time.Time
	done  chan struct{}
	short string
	ok    bool
	size  int // bytes counted against the budgets
}

// wait blocks until the entry's rewrite is decided; false when the request
// ended first.
func (e *memoEntry) wait(ctx context.Context) bool {
	select {
	case <-e.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// get returns a live entry, possibly still being decided.
func (m *memo) get(session string, key [32]byte, now time.Time) (*memoEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(now)
	sm := m.sessions[session]
	if sm == nil {
		return nil, false
	}
	el, ok := sm.entries[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*memoEntry)
	if now.Sub(e.at) > memoTTL {
		m.remove(sm, el)
		return nil, false
	}
	sm.lru.MoveToFront(el)
	sm.used = now
	m.order.MoveToFront(sm.el)
	return e, true
}

// reserve returns the entry for key, creating a pending one when there is
// none; leader says the caller created it and must finish or drop it, so two
// concurrent requests never make two different rewrites of one block.
func (m *memo) reserve(session string, key [32]byte, now time.Time) (*memoEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]*sessionMemo{}
	}
	sm := m.sessions[session]
	if sm == nil {
		sm = &sessionMemo{id: session, entries: map[[32]byte]*list.Element{}}
		sm.el = m.order.PushFront(sm)
		m.sessions[session] = sm
	}
	sm.used = now
	m.order.MoveToFront(sm.el)
	if el, ok := sm.entries[key]; ok {
		return el.Value.(*memoEntry), false
	}
	e := &memoEntry{key: key, at: now, done: make(chan struct{})}
	sm.entries[key] = sm.lru.PushFront(e)
	for sm.lru.Len() > memoPerSession {
		m.remove(sm, sm.lru.Back())
	}
	for len(m.sessions) > memoSessions {
		m.dropSession(m.order.Back().Value.(*sessionMemo))
	}
	return e, true
}

// finish decides a pending entry and counts it against the byte budget.
func (m *memo) finish(session string, e *memoEntry, short string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.short, e.ok = short, ok
	close(e.done)
	sm := m.sessions[session]
	if sm == nil {
		return
	}
	if el, live := sm.entries[e.key]; !live || el.Value.(*memoEntry) != e {
		return // evicted while the model was writing
	}
	e.size = len(short) + len(e.key)
	sm.bytes += e.size
	m.bytes += e.size
	for m.bytes > memoBytes && m.order.Len() > 0 {
		m.dropSession(m.order.Back().Value.(*sessionMemo))
	}
}

// drop removes a pending entry whose request ended, so a later request
// tries again, and releases anyone waiting on it with the original text.
func (m *memo) drop(session string, e *memoEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	close(e.done)
	if sm := m.sessions[session]; sm != nil {
		if el, live := sm.entries[e.key]; live && el.Value.(*memoEntry) == e {
			m.remove(sm, el)
		}
	}
}

// expire drops sessions unused for longer than the TTL. Callers hold mu.
func (m *memo) expire(now time.Time) {
	for m.order.Len() > 0 {
		sm := m.order.Back().Value.(*sessionMemo)
		if now.Sub(sm.used) <= memoTTL {
			return
		}
		m.dropSession(sm)
	}
}

// remove drops one entry. Callers hold mu.
func (m *memo) remove(sm *sessionMemo, el *list.Element) {
	e := sm.lru.Remove(el).(*memoEntry)
	delete(sm.entries, e.key)
	sm.bytes -= e.size
	m.bytes -= e.size
	if sm.lru.Len() == 0 {
		m.dropSession(sm)
	}
}

// dropSession forgets a session's memo. Callers hold mu.
func (m *memo) dropSession(sm *sessionMemo) {
	if m.sessions[sm.id] != sm {
		return
	}
	m.order.Remove(sm.el)
	delete(m.sessions, sm.id)
	m.bytes -= sm.bytes
}

// textSpan is one user text block: the byte range of its JSON string literal
// in the body and its decoded text.
type textSpan struct {
	start, end int
	text       string
	// newest marks blocks of the last message, when that is the user's.
	newest bool
}

var errShape = errors.New("unexpected request shape")

// userTexts finds the text blocks of user messages. It reads only valid JSON
// and refuses objects with a repeated key on the path it walks, so the
// gateway and the provider cannot disagree on which value counts.
func userTexts(body []byte) ([]textSpan, error) {
	if !json.Valid(body) {
		return nil, errShape
	}
	i := skipSpace(body, 0)
	top, err := objectFields(body, i)
	if err != nil {
		return nil, err
	}
	msgs, ok := top["messages"]
	if !ok || body[msgs.start] != '[' {
		return nil, nil
	}
	items, err := arrayItems(body, msgs.start)
	if err != nil {
		return nil, err
	}
	var out []textSpan
	for n, it := range items {
		if body[it.start] != '{' {
			continue
		}
		f, err := objectFields(body, it.start)
		if err != nil {
			return nil, err
		}
		if r, ok := f["role"]; !ok || stringValue(body, r) != "user" {
			continue
		}
		c, ok := f["content"]
		if !ok {
			continue
		}
		newest := n == len(items)-1
		switch body[c.start] {
		case '"':
			out = append(out, textSpan{start: c.start, end: c.end, text: stringValue(body, c), newest: newest})
		case '[':
			blocks, err := arrayItems(body, c.start)
			if err != nil {
				return nil, err
			}
			for _, b := range blocks {
				if body[b.start] != '{' {
					continue
				}
				bf, err := objectFields(body, b.start)
				if err != nil {
					return nil, err
				}
				t, ok := bf["text"]
				if ty, has := bf["type"]; !has || stringValue(body, ty) != "text" || !ok || body[t.start] != '"' {
					continue
				}
				out = append(out, textSpan{start: t.start, end: t.end, text: stringValue(body, t), newest: newest})
			}
		}
	}
	return out, nil
}

// valueSpan is the byte range of one JSON value.
type valueSpan struct{ start, end int }

// stringValue decodes a JSON string value; anything else is "".
func stringValue(body []byte, v valueSpan) string {
	if body[v.start] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(body[v.start:v.end], &s) != nil {
		return ""
	}
	return s
}

// objectFields maps the keys of the object starting at i to their values.
// body must be valid JSON (userTexts checks it once).
func objectFields(body []byte, i int) (map[string]valueSpan, error) {
	if i >= len(body) || body[i] != '{' {
		return nil, errShape
	}
	out := map[string]valueSpan{}
	j := skipSpace(body, i+1)
	if j < len(body) && body[j] == '}' {
		return out, nil
	}
	for j < len(body) {
		kend, err := valueEnd(body, j)
		if err != nil || body[j] != '"' {
			return nil, errShape
		}
		var key string
		if err := json.Unmarshal(body[j:kend], &key); err != nil {
			return nil, errShape
		}
		j = skipSpace(body, kend)
		if j >= len(body) || body[j] != ':' {
			return nil, errShape
		}
		j = skipSpace(body, j+1)
		vend, err := valueEnd(body, j)
		if err != nil {
			return nil, err
		}
		if _, dup := out[key]; dup {
			return nil, errShape
		}
		out[key] = valueSpan{j, vend}
		j = skipSpace(body, vend)
		if j >= len(body) {
			break
		}
		switch body[j] {
		case ',':
			j = skipSpace(body, j+1)
		case '}':
			return out, nil
		default:
			return nil, errShape
		}
	}
	return nil, errShape
}

// arrayItems lists the values of the array starting at i.
func arrayItems(body []byte, i int) ([]valueSpan, error) {
	if i >= len(body) || body[i] != '[' {
		return nil, errShape
	}
	var out []valueSpan
	j := skipSpace(body, i+1)
	if j < len(body) && body[j] == ']' {
		return out, nil
	}
	for j < len(body) {
		end, err := valueEnd(body, j)
		if err != nil {
			return nil, err
		}
		out = append(out, valueSpan{j, end})
		j = skipSpace(body, end)
		if j >= len(body) {
			break
		}
		switch body[j] {
		case ',':
			j = skipSpace(body, j+1)
		case ']':
			return out, nil
		default:
			return nil, errShape
		}
	}
	return nil, errShape
}

// valueEnd returns the index just past the value starting at i.
func valueEnd(body []byte, i int) (int, error) {
	if i >= len(body) {
		return 0, errShape
	}
	switch body[i] {
	case '"':
		return stringEnd(body, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(body); j++ {
			switch body[j] {
			case '"':
				end, err := stringEnd(body, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, errShape
	default:
		j := i
		for j < len(body) && !strings.ContainsRune(",]} \t\r\n", rune(body[j])) {
			j++
		}
		if j == i {
			return 0, errShape
		}
		return j, nil
	}
}

// stringEnd returns the index just past the string starting at i.
func stringEnd(body []byte, i int) (int, error) {
	for j := i + 1; j < len(body); j++ {
		switch body[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		}
	}
	return 0, errShape
}

func skipSpace(body []byte, i int) int {
	for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\r' || body[i] == '\n') {
		i++
	}
	return i
}
