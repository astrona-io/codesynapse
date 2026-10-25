package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
)

// `codesynapse mcp bridge` lets a client that cannot put its chat identity in
// HTTP headers use the MCP server with accounts. Claude Code starts one stdio
// server per chat and gives it the chat's ID in CLAUDE_CODE_SESSION_ID (it
// does not give that to remote servers or their headersHelper). The bridge
// registers that ID through the API, then forwards every JSON-RPC message to
// the streamable HTTP endpoint with the key and both runtime headers. It
// never invents an ID: without one it refuses to start.

const (
	// claudeSessionEnv is set by Claude Code for stdio MCP servers.
	claudeSessionEnv = "CLAUDE_CODE_SESSION_ID"
	maxBridgeMessage = 16 << 20
	// bridgeParallel bounds requests in flight after initialisation.
	bridgeParallel = 8
)

type mcpBridgeOptions struct {
	client   string
	EC       *app.Env
	url      string
	apiURL   string
	worktree bool
	repo     string
}

func (o *mcpBridgeOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.client, "client", "", "Runtime metadata adapter: codex or cursor (default: per-chat Claude bridge)")
	f.StringVar(&o.url, "url", app.Getenv("CODESYNAPSE_MCP_URL", "http://localhost:8090/mcp"), "MCP endpoint to forward to")
	f.StringVar(&o.apiURL, "api-url", app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"), "CodeSynapse API that registers the chat session")
	f.BoolVar(&o.worktree, "worktree", app.Getenv("CODESYNAPSE_BRIDGE_WORKTREE", "false") == "true", "Opt in: while this checkout has changes the server has not indexed, answer the core repository tools from a graph of the working tree built on this machine (those answers never reach the server, so they are not audited or counted)")
	f.StringVar(&o.repo, "repo", "", "The name the server knows this checkout by (default: from the origin remote)")
	return nil
}
func (o *mcpBridgeOptions) Complete(context.Context, *app.Env) error { return nil }
func (o *mcpBridgeOptions) Validate(context.Context, *app.Env) error {
	if o.client != "" && o.client != "codex" && o.client != "cursor" {
		return errors.New("--client must be codex or cursor")
	}
	if o.client != "" && o.worktree {
		return errors.New("runtime adapters require server-side answers; turn off --worktree")
	}
	for _, u := range []string{o.url, o.apiURL} {
		if err := checkBridgeURL(u); err != nil {
			return err
		}
	}
	return nil
}

// checkBridgeURL keeps the key off plain HTTP except to this machine.
func checkBridgeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%q must be an http(s) URL without credentials", raw)
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("%q: use https for hosts other than this machine; the key travels with every request", raw)
		}
	}
	return nil
}

func (o *mcpBridgeOptions) Run(ctx context.Context, _ *app.Env) error {
	if o.client != "" {
		c := newAPIClient(o.apiURL)
		if c.token == "" {
			return fmt.Errorf("no API key: run codesynapse mcp install with a personal key")
		}
		b := &bridge{url: o.url, token: c.token, out: o.EC.Stdout, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
		b.routes = &runtimeRoutes{client: o.client, api: c, owner: b, chats: map[string]*runtimeChat{}}
		if o.client == "cursor" {
			b.routes.primary = os.Getenv(cursorCLISessionEnv)
			if b.routes.primary == "" {
				var err error
				b.routes.primary, err = findCursorLaunch(ctx, cursorLaunchDir(), c.token)
				if err != nil {
					return err
				}
			}
			if b.routes.primary != "" && execution.Validate(execution.Identity{TenantID: "t", SubjectID: "u", SessionID: b.routes.primary, ParentSessionID: b.routes.primary}) != nil {
				return execution.ErrInvalid
			}
		}
		return b.run(ctx, o.EC.Stdin)
	}
	session := cmp.Or(os.Getenv(agentcfg.SessionEnv), os.Getenv(claudeSessionEnv))
	if parent := os.Getenv(agentcfg.ParentSessionEnv); parent != "" && parent != session {
		return execution.ErrConflict
	}
	if session == "" {
		return fmt.Errorf("no chat session: start the bridge from Claude Code (it sets %s) or set %s", claudeSessionEnv, agentcfg.SessionEnv)
	}
	c := newAPIClient(o.apiURL)
	if c.token == "" {
		return fmt.Errorf("no API key: set %s or run `codesynapse mcp install`", apiTokenEnv)
	}
	if _, err := c.executionContext(ctx, session, session); err != nil {
		return err
	}
	b := &bridge{url: o.url, token: c.token, session: session, out: o.EC.Stdout, report: c, logger: o.EC.Logger,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if o.worktree {
		if cwd, err := os.Getwd(); err == nil {
			b.worktree = newWorktree(ctx, cwd, o.repo, o.EC.Logger)
		}
	}
	return b.run(ctx, o.EC.Stdin)
}

type bridge struct {
	routes              *runtimeRoutes
	url, token, session string
	http                *http.Client
	out                 io.Writer
	outMu               sync.Mutex

	mu        sync.Mutex
	client    string // the client's own name from initialize, for usage counts
	mcpID     string // Mcp-Session-Id from the server
	protocol  string // negotiated protocol version
	readyOnce sync.Once
	readyCh   chan struct{} // closed once initialize was answered

	// worktree answers core tools from the working tree; nil forwards all.
	worktree *worktree

	// The git tools are answered here (gitbridge.go). report tells the API
	// about each call; gitDir is the checkout, found on first use.
	report apiClient
	logger *slog.Logger
	gitMu  sync.Mutex
	gitDir string

	// repoTools are the tools that take a repo argument (learned from
	// tools/list); repoName is the checkout's repository (bridgerepo.go).
	repoTools map[string]bool
	repoOnce  sync.Once
	repoName  string
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func (b *bridge) run(ctx context.Context, in io.Reader) error {
	b.readyCh = make(chan struct{})
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxBridgeMessage)
	slots := make(chan struct{}, bridgeParallel)
	var wg sync.WaitGroup
	initSeen := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		msg := append([]byte(nil), line...)
		var m rpcMessage
		_ = json.Unmarshal(msg, &m) //nolint:errcheck // the server answers malformed input itself
		if m.Method == "initialize" {
			b.noteClient(msg)
			if b.routes != nil {
				if initSeen {
					b.write(errorResponse(m.ID, errors.New("bridge already initialized")))
					continue
				}
				b.routes.init = append([]byte(nil), msg...)
			}
			// Everything else waits for the session the answer opens.
			initSeen = true
			b.forward(ctx, msg, m)
			continue
		}
		if !initSeen && len(m.ID) > 0 && m.Method != "" {
			// A request ahead of initialize is a client probing for a newer
			// protocol (Claude Code sends server/discover to stdio servers).
			// Answer it at once: waiting for initialize here would stop this
			// loop from ever reading it, and the client from ever connecting.
			b.write(methodNotFound(m.ID))
			continue
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			select {
			case <-b.readyCh:
			case <-ctx.Done():
				return
			}
			if m.Method == "tools/call" && (b.answerGit(ctx, msg, m) || b.answerLocally(ctx, msg, m)) {
				return
			}
			msg := b.withRepo(ctx, msg, m)
			if b.routes != nil && m.Method == "tools/call" {
				target, err := b.routes.forCall(ctx, msg)
				if err != nil {
					if len(m.ID) > 0 {
						b.write(errorResponse(m.ID, err))
					}
					return
				}
				target.forward(ctx, msg, m)
				return
			}
			b.forward(ctx, msg, m)
		}()
	}
	wg.Wait()
	if b.routes != nil {
		b.routes.close(ctx)
	}
	b.close(ctx)
	return sc.Err()
}

// answerLocally answers a core tool call from the working tree when it
// applies, and reports whether it did.
func (b *bridge) answerLocally(ctx context.Context, msg []byte, m rpcMessage) bool {
	c, ok := b.worktree.local(msg)
	if !ok || len(m.ID) == 0 {
		return false
	}
	result, err := b.worktree.answer(ctx, c)
	if err != nil {
		if !errors.Is(err, errNoChanges) {
			b.worktree.logger.Warn("answering from the server instead of the working tree", "tool", c.Params.Name, "error", err)
		}
		return false
	}
	out, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	if err != nil {
		return false
	}
	b.write(out)
	return true
}

// forward posts one message and writes whatever the server answers. A failed
// request still gets a JSON-RPC error, so the client never waits forever.
func (b *bridge) forward(ctx context.Context, msg []byte, m rpcMessage) {
	if err := b.post(ctx, msg, m); err != nil && len(m.ID) > 0 && m.Method != "" {
		b.write(errorResponse(m.ID, err))
	}
	if m.Method == "initialize" {
		b.readyOnce.Do(func() { close(b.readyCh) })
	}
}

// clientHeader carries the MCP client's name from initialize on every
// forwarded request: the server counts usage per client, and requests that
// arrive without the session's initialize (every call after the first
// through the streamable transport) would otherwise count as "unknown".
const clientHeader = "X-CodeSynapse-Client"

var clientNameRe = regexp.MustCompile(`[^a-z0-9._-]+`)

// noteClient keeps the clientInfo name of an initialize message, lower-cased
// and reduced to the characters usage client names allow.
func (b *bridge) noteClient(msg []byte) {
	var init struct {
		Params struct {
			ClientInfo struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	if json.Unmarshal(msg, &init) != nil {
		return
	}
	name := clientNameRe.ReplaceAllString(strings.ToLower(init.Params.ClientInfo.Name), "-")
	if len(name) > 64 {
		name = name[:64]
	}
	b.mu.Lock()
	b.client = strings.Trim(name, "-")
	b.mu.Unlock()
}

func (b *bridge) headers(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	if b.session != "" {
		req.Header.Set(execution.SessionHeader, b.session)
		req.Header.Set(execution.ParentSessionHeader, b.session)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mcpID != "" {
		req.Header.Set("Mcp-Session-Id", b.mcpID)
	}
	if b.client != "" {
		req.Header.Set(clientHeader, b.client)
	}
	if b.protocol != "" {
		req.Header.Set("Mcp-Protocol-Version", b.protocol)
	}
}

func (b *bridge) post(ctx context.Context, msg []byte, m rpcMessage) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	b.headers(req)
	res, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // read-only response
	if id := res.Header.Get("Mcp-Session-Id"); id != "" {
		b.mu.Lock()
		b.mcpID = id
		b.mu.Unlock()
	}
	body := io.LimitReader(res.Body, maxBridgeMessage)
	if res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(body, 4<<10)) //nolint:errcheck // best-effort detail
		return fmt.Errorf("CodeSynapse answered %d: %s", res.StatusCode, strings.TrimSpace(string(data)))
	}
	if res.StatusCode == http.StatusAccepted {
		return nil
	}
	ct, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")) //nolint:errcheck // empty means JSON
	if ct == "text/event-stream" {
		return readEvents(body, func(data []byte) { b.deliver(data, m) })
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) > 0 {
		b.deliver(data, m)
	}
	return nil
}

// deliver writes one server message and notes the protocol version the
// initialize answer settles on.
func (b *bridge) deliver(data []byte, sent rpcMessage) {
	if sent.Method == "tools/list" {
		b.learnRepoTools(data)
	}
	if sent.Method == "initialize" {
		var r struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(data, &r) == nil && r.Result.ProtocolVersion != "" {
			b.mu.Lock()
			b.protocol = r.Result.ProtocolVersion
			b.mu.Unlock()
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, data) == nil {
		b.write(compact.Bytes())
	}
}

func (b *bridge) write(line []byte) {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.out.Write(append(line, '\n')) //nolint:errcheck // stdout closing ends the client anyway
}

// close ends the MCP transport session. The chat's runtime session stays
// registered: Claude Code resumes a chat with the same ID.
func (b *bridge) close(ctx context.Context) {
	b.mu.Lock()
	id := b.mcpID
	b.mu.Unlock()
	if id == "" {
		return
	}
	// Still close when the client already cancelled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.url, nil)
	if err != nil {
		return
	}
	b.headers(req)
	if res, err := b.http.Do(req); err == nil {
		_ = res.Body.Close() //nolint:errcheck // best effort
	}
}

// readEvents calls fn with the data of each server-sent event.
func readEvents(r io.Reader, fn func([]byte)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxBridgeMessage)
	var data []byte
	flush := func() {
		if len(data) > 0 {
			fn(data)
		}
		data = nil
	}
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			flush()
		case bytes.HasPrefix(line, []byte("data:")):
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))...)
		}
	}
	flush()
	return sc.Err()
}

// methodNotFound is the JSON-RPC answer to a request this server does not
// handle, which tells a probing client to fall back to initialize.
func methodNotFound(id json.RawMessage) []byte {
	out, _ := json.Marshal(map[string]any{ //nolint:errchkjson // fixed shape
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32601, "message": "Method not found"},
	})
	return out
}

func errorResponse(id json.RawMessage, err error) []byte {
	out, _ := json.Marshal(map[string]any{ //nolint:errchkjson // fixed shape
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32603, "message": firstLine(err.Error(), 500)},
	})
	return out
}

func firstLine(s string, limit int) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}
