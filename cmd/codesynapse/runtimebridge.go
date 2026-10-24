package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/astrona-io/codesynapse/internal/execution"
)

const maxRuntimeChats = 128

// One native MCP process can serve several chats. Only discovery uses the
// unbound transport; every invocation gets an independently initialized,
// authenticated upstream transport pinned to its real runtime identity.
type runtimeRoutes struct {
	primary string // only a Cursor CLI process started with a runtime-owned chat ID
	client  string
	api     apiClient
	owner   *bridge
	init    []byte
	mu      sync.Mutex
	chats   map[string]*runtimeChat
}

type runtimeChat struct {
	ready  chan struct{}
	bridge *bridge
	err    error
}

func (r *runtimeRoutes) identity(msg []byte) (string, error) {
	var call struct {
		Params struct {
			Meta struct {
				ThreadID      string          `json:"threadId"`
				ProgressToken json.RawMessage `json:"progressToken"`
				Codex         struct {
					ThreadID string `json:"thread_id"`
					Parent   string `json:"parent_thread_id"`
				} `json:"x-codex-turn-metadata"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if json.Unmarshal(msg, &call) != nil {
		return "", execution.ErrInvalid
	}
	m := call.Params.Meta
	var primary string
	if r.client == "codex" {
		primary = m.Codex.ThreadID
		if primary == "" {
			primary = m.ThreadID
		}
		if m.ThreadID != "" && m.ThreadID != primary {
			return "", execution.ErrConflict
		}
		if m.Codex.Parent != "" {
			var err error
			primary, err = readRuntimeReceipt(r.api, "codex", "thread", primary, false)
			if err != nil {
				return "", fmt.Errorf("codex subagent identity unavailable: review /hooks and restart the chat: %w", err)
			}
		}
	} else {
		if r.primary != "" {
			if _, err := readRuntimeReceipt(r.api, "cursor", "thread", "cli-mismatch:"+r.primary, false); !errors.Is(err, os.ErrNotExist) {
				return "", errors.New("cursor changed chats inside this process; restart with codesynapse cursor")
			}
			return r.primary, nil
		}
		var callID string
		if json.Unmarshal(m.ProgressToken, &callID) != nil || callID == "" {
			return "", errors.New("cursor did not supply an MCP call identity; start Cursor CLI with `codesynapse cursor`, or update the desktop editor and enable CodeSynapse hooks")
		}
		var err error
		primary, err = readRuntimeReceipt(r.api, "cursor", "call", callID, true)
		if err != nil {
			return "", fmt.Errorf("cursor call identity unavailable: enable CodeSynapse beforeMCPExecution hooks and retry: %w", err)
		}
	}
	if execution.Validate(execution.Identity{TenantID: "t", SubjectID: "u", SessionID: primary, ParentSessionID: primary}) != nil {
		return "", errors.New("runtime thread metadata missing or invalid; update the assistant and restart the MCP connection")
	}
	return primary, nil
}

func (r *runtimeRoutes) forCall(ctx context.Context, msg []byte) (*bridge, error) {
	id, err := r.identity(msg)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	chat, exists := r.chats[id]
	if !exists {
		if len(r.chats) >= maxRuntimeChats {
			r.mu.Unlock()
			return nil, errors.New("runtime bridge chat limit reached; restart the MCP connection")
		}
		chat = &runtimeChat{ready: make(chan struct{})}
		r.chats[id] = chat
	}
	r.mu.Unlock()
	if !exists {
		chat.bridge, chat.err = r.initialize(ctx, id)
		close(chat.ready)
		if chat.err != nil {
			r.mu.Lock()
			if r.chats[id] == chat {
				delete(r.chats, id)
			}
			r.mu.Unlock()
		}
	}
	select {
	case <-chat.ready:
		return chat.bridge, chat.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *runtimeRoutes) initialize(ctx context.Context, id string) (*bridge, error) {
	if _, err := r.api.executionContext(ctx, id, id); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	b := &bridge{url: r.owner.url, token: r.api.token, session: id, http: r.owner.http, out: &output}
	initialized := false
	defer func() {
		if !initialized {
			b.close(ctx)
		}
	}()

	var init rpcMessage
	if json.Unmarshal(r.init, &init) != nil || init.Method != "initialize" {
		return nil, errors.New("MCP initialize required before invocation")
	}
	if err := b.post(ctx, r.init, init); err != nil {
		return nil, err
	}
	var reply struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		Error json.RawMessage
	}
	if json.Unmarshal(bytes.TrimSpace(output.Bytes()), &reply) != nil || len(reply.Error) > 0 || reply.Result.ProtocolVersion == "" {
		return nil, errors.New("upstream MCP initialization failed")
	}
	output.Reset()
	ready := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if err := b.post(ctx, ready, rpcMessage{Method: "notifications/initialized"}); err != nil {
		return nil, err
	}
	b.out = runtimeOutput{r.owner}
	initialized = true
	return b, nil
}

// All child bridges share one stdout lock so concurrent replies stay JSONL.
type runtimeOutput struct{ owner *bridge }

func (w runtimeOutput) Write(b []byte) (int, error) {
	w.owner.outMu.Lock()
	defer w.owner.outMu.Unlock()
	return w.owner.out.Write(b)
}

func (r *runtimeRoutes) close(ctx context.Context) {
	var wg sync.WaitGroup
	for _, chat := range r.chats {
		if chat.bridge != nil {
			wg.Add(1)
			go func(b *bridge) { defer wg.Done(); b.close(ctx) }(chat.bridge)
		}
	}
	wg.Wait()
}
