package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
)

// A CodeSynapse server that is stopped, restarting or unreachable must not
// cost a Claude Code chat its tools: Claude Code remembers a stdio server
// that exited or failed to connect for the rest of the chat. So the bridge
// starts and answers without the server. initialize and tools/list come from
// the last answers kept on disk, every tool call gets offlineText at once,
// and a background probe brings the chat back as soon as the server answers,
// with notifications/tools/list_changed so the client fetches the real list.
// Nothing is forwarded or answered locally before the chat's runtime session
// is registered, and a key or session the API refuses stays refused.

// offlineText answers every tool call while the server cannot be reached.
const offlineText = "CodeSynapse is offline (server unreachable); use Read/Grep/Bash for this step. It reconnects automatically."

// offlineInitText starts the server instructions of an offline initialize.
const offlineInitText = "CodeSynapse is offline (server unreachable): use Read, Grep and Bash until a CodeSynapse tool call succeeds. It reconnects automatically, without restarting the chat."

// errOffline is what send returns when the server could not be reached; the
// caller answers with offlineText.
var errOffline = errors.New("CodeSynapse server unreachable")

// errHeaderTimeout means the server accepted the request but sent nothing.
var errHeaderTimeout = errors.New("CodeSynapse did not answer in time")

// errNotAlive means a tool call waited without an answer and a liveness check
// on a fresh connection did not reach the server either.
var errNotAlive = fmt.Errorf("%w: a liveness check did not reach the server", errHeaderTimeout)

const (
	bridgeDialTimeout = 5 * time.Second
	// bridgeHeaderTimeout bounds the wait for an answer to anything but a
	// tool call; bridgeInitHeaderTimeout the wait for initialize.
	bridgeHeaderTimeout     = 30 * time.Second
	bridgeInitHeaderTimeout = 10 * time.Second
	// maxBridgeCache bounds a kept answer.
	maxBridgeCache = 2 << 20
	// offlineFresh is how long hooks trust an offline marker; the probe
	// rewrites it while the outage lasts.
	offlineFresh = 2 * time.Minute
	// refusedFresh is how long a refused chat stays marked.
	refusedFresh = 12 * time.Hour
)

// Variables so tests can shorten them.
var (
	// bridgeInitWait is how long initialize waits for the registration
	// before it answers offline. Claude Code gives a server 30 s.
	bridgeInitWait = 10 * time.Second
	probeMinDelay  = time.Second
	probeMaxDelay  = 15 * time.Second
	// probeTimeout bounds one background attempt to reach the server.
	probeTimeout = 10 * time.Second
	// callLiveWait is how long a tool call waits for headers before a
	// liveness check; aliveTimeout bounds that check.
	callLiveWait = 20 * time.Second
	aliveTimeout = 5 * time.Second
)

// alive asks the server, on a connection of its own, whether it answers at
// all. Any HTTP answer counts; only an error that unreachable accepts means
// it is gone.
func (b *bridge) alive(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, aliveTimeout)
	defer cancel()
	var t *http.Transport
	if own, ok := b.http.Transport.(*http.Transport); ok {
		t = own.Clone()
	} else {
		t = newBridgeHTTP().Transport.(*http.Transport).Clone() //nolint:forcetypeassert // built just above
	}
	t.DisableKeepAlives = true
	defer t.CloseIdleConnections()
	ping := []byte(`{"jsonrpc":"2.0","id":"codesynapse-bridge-alive","method":"ping"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(ping))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	b.headers(req)
	res, err := (&http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// offlineProtocols are the versions an offline initialize may agree to: those
// the server's SDK negotiates, so the replayed initialize agrees on the same.
var offlineProtocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const defaultOfflineProtocol = "2025-11-25"

// mcpStatusError is an answer of the MCP server other than success.
type mcpStatusError struct {
	Status int
	Detail string
}

func (e *mcpStatusError) Error() string {
	return fmt.Sprintf("CodeSynapse answered %d: %s", e.Status, e.Detail)
}

func headerWait(method string) time.Duration {
	switch method {
	case "tools/call":
		return bridgeCallLimit
	case "initialize":
		return bridgeInitHeaderTimeout
	}
	return bridgeHeaderTimeout
}

// newBridgeHTTP gives up quickly on a host that does not accept connections.
func newBridgeHTTP() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's own type
	t.DialContext = (&net.Dialer{Timeout: bridgeDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = bridgeDialTimeout
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// unreachable reports whether err means the server could not be reached or
// did not answer, as opposed to an answer that refused the request.
func unreachable(err error) bool {
	if err == nil {
		return false
	}
	var mse *mcpStatusError
	if errors.As(err, &mse) {
		return gatewayStatus(mse.Status)
	}
	var se *statusError
	if errors.As(err, &se) {
		return gatewayStatus(se.Status)
	}
	var ne net.Error
	var ue *url.Error
	return errors.Is(err, errHeaderTimeout) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.As(err, &ne) || errors.As(err, &ue)
}

// gatewayStatus is what a proxy answers for a server behind it that is down.
func gatewayStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// refusedForGood reports whether an error is an answer that retrying will not
// change: a refused key or session (401, 403) or an unusable chat identity.
// Any other answer (a 404 or 400 from an ingress during an upgrade, a 500) is
// a failed attempt: tried again, never a reason to give up on the chat.
func refusedForGood(err error) bool {
	if errors.Is(err, execution.ErrMissing) || errors.Is(err, execution.ErrConflict) || errors.Is(err, execution.ErrInvalid) {
		return true
	}
	if se := (*statusError)(nil); errors.As(err, &se) {
		return se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden
	}
	if mse := (*mcpStatusError)(nil); errors.As(err, &mse) {
		return mse.Status == http.StatusUnauthorized || mse.Status == http.StatusForbidden
	}
	return false
}

// runtimeSessionLost reports whether the MCP server no longer knows the
// chat's runtime session (expired, revoked, a fresh accounts database), which
// registering it again may fix.
func runtimeSessionLost(err error) bool {
	var mse *mcpStatusError
	return errors.As(err, &mse) && mse.Status == http.StatusForbidden && strings.Contains(mse.Detail, "runtime session is unavailable")
}

func refusalText(err error) string {
	return "CodeSynapse refused this chat (" + firstLine(err.Error(), 200) + "); use Read/Grep/Bash. Check the API key, then restart the chat to reconnect."
}

// root is the bridge that holds the online state: a runtime adapter's
// per-chat bridges share their parent's.
func (b *bridge) root() *bridge {
	if b.parent != nil {
		return b.parent
	}
	return b
}

// needsRegistration reports whether calls wait for a registered session.
func (b *bridge) needsRegistration() bool {
	return b.parent == nil && b.session != "" && b.report.base != ""
}

// start begins registering the chat's runtime session.
func (b *bridge) start(ctx context.Context) {
	b.runCtx = ctx
	b.kick = make(chan struct{}, 1)
	b.regCh = make(chan struct{})
	if !b.needsRegistration() {
		b.settleRegistration()
		return
	}
	b.mu.Lock()
	b.probing = true
	b.mu.Unlock()
	b.probes.Go(func() { b.probeLoop(ctx) })
}

func (b *bridge) settleRegistration() {
	if b.regCh != nil {
		b.regOnce.Do(func() { close(b.regCh) })
	}
}

// register registers the chat's runtime session. A refusal is kept: every
// later call is answered with it.
func (b *bridge) register(ctx context.Context) error {
	b.regMu.Lock()
	defer b.regMu.Unlock()
	rctx, err := b.report.executionContext(ctx, b.session, b.session)
	if err != nil {
		if refusedForGood(err) {
			b.mu.Lock()
			b.regErr = err
			b.mu.Unlock()
			b.writeMarker(true)
			b.settleRegistration()
		}
		return err
	}
	id, _ := execution.From(rctx)
	b.mu.Lock()
	b.ident = &id
	b.mu.Unlock()
	b.settleRegistration()
	return nil
}

// waitRegistered waits up to d for the registration to settle.
func (b *bridge) waitRegistered(ctx context.Context, d time.Duration) bool {
	if b.regCh == nil {
		return true
	}
	b.kickProbe()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-b.regCh:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// callCtx carries the registered session, so reports of local answers name
// the chat.
func (b *bridge) callCtx(ctx context.Context) context.Context {
	b.mu.Lock()
	id := b.ident
	b.mu.Unlock()
	if id == nil {
		return ctx
	}
	if cctx, err := execution.With(ctx, *id); err == nil {
		return cctx
	}
	return ctx
}

// unavailable returns the text calls are answered with while they may not go
// to the server, or "" when they may.
func (b *bridge) unavailable() string {
	r := b.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.regErr != nil:
		return refusalText(r.regErr)
	case r.offline || r.needsRegistration() && r.ident == nil:
		return offlineText
	}
	return ""
}

// initialize answers the client's initialize: from the server when it can be
// reached, else from the bridge, so the client connects either way.
func (b *bridge) initialize(ctx context.Context, msg []byte, m rpcMessage) {
	defer b.readyOnce.Do(func() { close(b.readyCh) })
	b.mu.Lock()
	b.initMsg = msg
	b.mu.Unlock()
	if !b.waitRegistered(ctx, bridgeInitWait) {
		b.markOffline()
		b.answerInit(msg, m, offlineInitText)
		return
	}
	if text := b.unavailable(); text != "" {
		if text == offlineText {
			text = offlineInitText
		}
		b.answerInit(msg, m, text)
		return
	}
	err := b.send(ctx, msg, m)
	switch {
	case err == nil:
	case errors.Is(err, errOffline):
		b.answerInit(msg, m, offlineInitText)
	case len(m.ID) > 0:
		b.write(errorResponse(m.ID, err))
	}
}

// answerInit answers initialize from the bridge: the last server answer kept
// on disk, or a minimal one, with text first in the instructions. The server
// gets the client's initialize once it can be reached (reinit).
func (b *bridge) answerInit(msg []byte, m rpcMessage, text string) {
	b.mu.Lock()
	b.needInit = true
	b.mu.Unlock()
	// Whatever the client lists before the server is back is the kept copy or
	// nothing, so it is told to list again then.
	r := b.root()
	r.mu.Lock()
	r.listStale = true
	r.mu.Unlock()
	if len(m.ID) == 0 {
		return
	}
	result := map[string]json.RawMessage{}
	if cached := b.cached("initialize"); cached != nil {
		_ = json.Unmarshal(cached, &result) //nolint:errcheck // a damaged copy means the minimal answer
	}
	var req struct {
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	_ = json.Unmarshal(msg, &req) //nolint:errcheck // no version asked means the default
	protocol := defaultOfflineProtocol
	if slices.Contains(offlineProtocols, req.Params.ProtocolVersion) {
		protocol = req.Params.ProtocolVersion
	}
	set := func(key string, v any) {
		if raw, err := json.Marshal(v); err == nil {
			result[key] = raw
		}
	}
	set("protocolVersion", protocol)
	caps := map[string]json.RawMessage{}
	_ = json.Unmarshal(result["capabilities"], &caps) //nolint:errcheck // missing means none
	// The client must listen for list_changed: that is how it learns the
	// tools once the server is back.
	caps["tools"] = json.RawMessage(`{"listChanged":true}`)
	set("capabilities", caps)
	if _, ok := result["serverInfo"]; !ok {
		set("serverInfo", map[string]string{"name": "codesynapse", "version": version})
	}
	var instructions string
	_ = json.Unmarshal(result["instructions"], &instructions) //nolint:errcheck // missing means none
	if instructions != "" {
		text += "\n\n" + instructions
	}
	set("instructions", text)
	out, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	if err == nil {
		b.write(out)
	}
}

// dispatch answers one message after initialize.
func (b *bridge) dispatch(ctx context.Context, msg []byte, m rpcMessage) {
	if text := b.unavailable(); text != "" {
		b.kickProbe()
		b.answerUnavailable(m, text)
		return
	}
	ctx = b.callCtx(ctx)
	if m.Method == "tools/call" && (b.answerGit(ctx, msg, m) || b.answerAskLocal(ctx, msg, m) || b.answerFetchSource(ctx, msg, m) || b.answerLocally(ctx, msg, m)) {
		return
	}
	msg = b.withRepo(ctx, msg, m)
	if b.routes != nil && m.Method == "tools/call" {
		target, err := b.routes.forCall(ctx, msg)
		if err != nil {
			if len(m.ID) == 0 {
				return
			}
			if ctx.Err() == nil && unreachable(err) {
				b.markOffline()
				b.answerUnavailable(m, offlineText)
				return
			}
			b.write(errorResponse(m.ID, err))
			return
		}
		target.forward(ctx, msg, m)
		return
	}
	b.forward(ctx, msg, m)
}

// answerUnavailable answers a message the server cannot take: a tool call
// with an error result the agent reads, discovery from the kept copy (or
// empty), anything else with a JSON-RPC error. Notifications are dropped.
func (b *bridge) answerUnavailable(m rpcMessage, text string) {
	if len(m.ID) == 0 || m.Method == "" {
		return
	}
	var result any
	switch m.Method {
	case "tools/call":
		result = map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": true}
	case "tools/list":
		result = map[string]any{"tools": []any{}}
		if text == offlineText {
			if tools := b.cached("tools"); tools != nil {
				result = tools
				b.learnRepoTools(mustResult(tools))
			}
			r := b.root()
			r.mu.Lock()
			r.listStale = true
			r.mu.Unlock()
		}
	case "ping":
		result = map[string]any{}
	case "prompts/list":
		result = map[string]any{"prompts": []any{}}
	case "resources/list":
		result = map[string]any{"resources": []any{}}
	case "resources/templates/list":
		result = map[string]any{"resourceTemplates": []any{}}
	default:
		b.write(errorResponse(m.ID, errors.New(text)))
		return
	}
	out, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	if err == nil {
		b.write(out)
	}
}

// mustResult wraps a kept result as the message learnRepoTools reads.
func mustResult(result json.RawMessage) []byte {
	out, _ := json.Marshal(map[string]json.RawMessage{"result": result}) //nolint:errchkjson // raw JSON
	return out
}

// send posts one message for this chat. A new server session is opened
// first when the server has none, and once more when the server forgot the
// one in use (a restart answers 404); a runtime session the server does not
// know is registered again. A server that cannot be reached marks the bridge
// offline and returns errOffline.
func (b *bridge) send(ctx context.Context, msg []byte, m rpcMessage) error {
	if m.Method != "initialize" {
		b.mu.Lock()
		need := b.needInit
		b.mu.Unlock()
		if need {
			if err := b.reinit(ctx, ""); err != nil {
				return b.settle(ctx, err)
			}
		}
	}
	b.mu.Lock()
	used := b.mcpID
	b.mu.Unlock()
	answered, err := b.postAnswer(ctx, msg, m)
	var se *mcpStatusError
	if err != nil && !answered && m.Method != "initialize" && errors.As(err, &se) {
		retry := false
		switch {
		case se.Status == http.StatusNotFound && used != "":
			if rerr := b.reinit(ctx, used); rerr != nil {
				return b.settle(ctx, rerr)
			}
			retry = true
		case runtimeSessionLost(err) && b.needsRegistration():
			if rerr := b.register(ctx); rerr != nil {
				return b.settle(ctx, rerr)
			}
			retry = true
		}
		if retry {
			answered, err = b.postAnswer(ctx, msg, m)
		}
	}
	if answered {
		// The client has its answer; a stream that broke off after it is no
		// reason to send a second one for the same id.
		return b.settle(ctx, nil)
	}
	return b.settle(ctx, err)
}

// postAnswer posts msg, writes what comes back to the client and reports
// whether the response to m was among it.
func (b *bridge) postAnswer(ctx context.Context, msg []byte, m rpcMessage) (bool, error) {
	answered := false
	err := b.postTo(ctx, msg, m, func(data []byte) {
		if len(m.ID) > 0 && !answered {
			var r struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(data, &r) == nil && bytes.Equal(bytes.TrimSpace(r.ID), bytes.TrimSpace(m.ID)) && (r.Result != nil || r.Error != nil) {
				answered = true
			}
		}
		b.deliver(data, m)
	})
	return answered, err
}

// settle records what a request says about the server's availability.
func (b *bridge) settle(ctx context.Context, err error) error {
	if err == nil {
		b.markOnline(false)
		return nil
	}
	if ctx.Err() == nil && unreachable(err) {
		b.markOffline()
		return errOffline
	}
	return err
}

// reinit opens a new server session for this chat by replaying the client's
// initialize and notifications/initialized; the answers stay in the bridge.
// used is the session that failed: when another call already replaced it,
// there is nothing to do.
func (b *bridge) reinit(ctx context.Context, used string) error {
	b.reinitMu.Lock()
	defer b.reinitMu.Unlock()
	b.mu.Lock()
	if !b.needInit && (used == "" || b.mcpID != used) {
		b.mu.Unlock()
		return nil
	}
	init := b.initMsg
	b.mcpID, b.protocol, b.needInit = "", "", true
	b.mu.Unlock()
	var m rpcMessage
	if init == nil || json.Unmarshal(init, &m) != nil || m.Method != "initialize" {
		return errors.New("MCP initialize required before invocation")
	}
	var reply struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	answered := false
	err := b.postTo(ctx, init, m, func(data []byte) {
		if !answered && json.Unmarshal(data, &reply) == nil && (reply.Result.ProtocolVersion != "" || len(reply.Error) > 0) {
			answered = true
			if len(reply.Error) == 0 {
				b.remember("initialize", data)
			}
		}
	})
	if err != nil {
		return err
	}
	if !answered || len(reply.Error) > 0 {
		return errors.New("CodeSynapse did not open a new MCP session")
	}
	b.mu.Lock()
	b.protocol = reply.Result.ProtocolVersion
	b.mu.Unlock()
	ready := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if err := b.postTo(ctx, ready, rpcMessage{Method: "notifications/initialized"}, func([]byte) {}); err != nil {
		return err
	}
	b.mu.Lock()
	b.needInit = false
	b.mu.Unlock()
	return nil
}

// markOffline notes that the server cannot be reached and starts probing.
func (b *bridge) markOffline() {
	r := b.root()
	r.mu.Lock()
	r.offline = true
	begin := !r.probing && r.runCtx != nil
	if begin {
		r.probing = true
	}
	r.mu.Unlock()
	r.writeMarker(false)
	if begin {
		r.probes.Go(func() { r.probeLoop(r.runCtx) })
	}
}

// markOnline notes that the server answered. A client that was given the
// kept tool list (or none) is told to fetch it again.
func (b *bridge) markOnline(endProbe bool) {
	r := b.root()
	r.mu.Lock()
	was, stale := r.offline, r.listStale
	r.offline, r.listStale = false, false
	if endProbe {
		r.probing = false
	}
	r.mu.Unlock()
	if was {
		r.clearMarker()
	}
	if stale {
		r.write([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
	}
}

// kickProbe makes the probe try now, and starts one when none runs.
func (b *bridge) kickProbe() {
	r := b.root()
	r.mu.Lock()
	begin := r.offline && !r.probing && r.runCtx != nil
	if begin {
		r.probing = true
	}
	r.mu.Unlock()
	if begin {
		r.probes.Go(func() { r.probeLoop(r.runCtx) })
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// probeLoop registers the session and reaches the server again, backing off
// up to probeMaxDelay, until it succeeds, the session is refused or the
// bridge stops.
func (b *bridge) probeLoop(ctx context.Context) {
	delay := probeMinDelay
	for {
		done, err := b.recover(ctx)
		if done {
			if err == nil {
				b.markOnline(true)
			} else {
				b.mu.Lock()
				b.probing = false
				b.mu.Unlock()
			}
			return
		}
		if b.logger != nil {
			b.logger.Debug("CodeSynapse still unreachable", "error", err)
		}
		b.mu.Lock()
		off := b.offline
		b.mu.Unlock()
		if off {
			b.writeMarker(false)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			b.mu.Lock()
			b.probing = false
			b.mu.Unlock()
			return
		case <-t.C:
		case <-b.kick:
			t.Stop()
		}
		delay = min(delay*2, probeMaxDelay)
	}
}

// recover makes one attempt. done means stop probing: with a nil error the
// server is back (or was never known to be down), else the chat is refused.
func (b *bridge) recover(ctx context.Context) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	b.mu.Lock()
	refused, registered := b.regErr, b.ident != nil || !b.needsRegistration()
	b.mu.Unlock()
	if refused != nil {
		return true, refused
	}
	if !registered {
		if err := b.register(pctx); err != nil {
			if refusedForGood(err) {
				return true, err
			}
			return false, err
		}
	}
	b.mu.Lock()
	off, need, used := b.offline, b.needInit, b.mcpID
	b.mu.Unlock()
	if !off && !need {
		return true, nil
	}
	attempt := func() error { return b.reinit(pctx, "") }
	if !need {
		attempt = func() error {
			ping := []byte(`{"jsonrpc":"2.0","id":"codesynapse-bridge-probe","method":"ping"}`)
			err := b.postTo(pctx, ping, rpcMessage{Method: "ping"}, func([]byte) {})
			var se *mcpStatusError
			if errors.As(err, &se) && se.Status == http.StatusNotFound && used != "" {
				err = b.reinit(pctx, used)
			}
			return err
		}
	}
	err := attempt()
	if runtimeSessionLost(err) && b.needsRegistration() {
		// The runtime session ended during the outage (expiry, revocation, a
		// fresh accounts database): register it again, as send does.
		if rerr := b.register(pctx); rerr != nil {
			return b.probeOutcome(rerr)
		}
		err = attempt()
	}
	return b.probeOutcome(err)
}

// probeOutcome sorts what one probe attempt ended with. Only an unreachable
// server keeps the probe going. A refused key or session is the chat's
// refusal from now on. Any other answer means the server is there: the bridge
// goes online, and the next call shows the error to the agent.
func (b *bridge) probeOutcome(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case unreachable(err):
		return false, err
	case refusedForGood(err):
		b.mu.Lock()
		if b.regErr == nil {
			b.regErr = err
		}
		b.mu.Unlock()
		b.writeMarker(true)
		b.settleRegistration()
		return true, err
	}
	if b.logger != nil {
		b.logger.Debug("CodeSynapse answered the probe with an error", "error", err)
	}
	return true, nil
}

// The kept copy of the last initialize and tools/list answers, per server
// URL, so a chat that starts while the server is down still lists its tools.
// It is keyed by URL only and holds no secrets: the instructions and tool
// list the server gives its clients. The URL carries no credentials
// (checkBridgeURL).
type bridgeCache struct {
	URL        string          `json:"url"`
	Initialize json.RawMessage `json:"initialize,omitempty"`
	Tools      json.RawMessage `json:"tools,omitempty"`
}

// bridgeCacheRoot is where the kept answers and offline markers live; tests
// point it at a temporary folder.
var bridgeCacheRoot = os.UserCacheDir

func bridgeCacheDir() string {
	dir, err := bridgeCacheRoot()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "codesynapse", "bridge")
}

func (b *bridge) cacheFile() string {
	if b.cacheDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(b.url))
	return filepath.Join(b.cacheDir, hex.EncodeToString(sum[:8])+".json")
}

func (b *bridge) loadCache() (bridgeCache, bool) {
	file := b.cacheFile()
	if file == "" {
		return bridgeCache{}, false
	}
	f, err := os.Open(file) //nolint:gosec // our own cache file
	if err != nil {
		return bridgeCache{}, false
	}
	defer f.Close() //nolint:errcheck // read-only
	var c bridgeCache
	if json.NewDecoder(io.LimitReader(f, 2*maxBridgeCache+4096)).Decode(&c) != nil || c.URL != b.url {
		return bridgeCache{}, false
	}
	return c, true
}

// cached returns the kept result of kind ("initialize" or "tools").
func (b *bridge) cached(kind string) json.RawMessage {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	c, ok := b.loadCache()
	if !ok {
		return nil
	}
	if kind == "tools" {
		return c.Tools
	}
	return c.Initialize
}

// remember keeps the result of a successful initialize or tools/list answer.
func (b *bridge) remember(kind string, data []byte) {
	if b.cacheFile() == "" || len(data) > maxBridgeCache {
		return
	}
	var msg struct {
		Result map[string]json.RawMessage `json:"result"`
		Error  json.RawMessage            `json:"error"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.Result == nil || len(msg.Error) > 0 {
		return
	}
	// A kept page must not send the client after a next one.
	delete(msg.Result, "nextCursor")
	result, err := json.Marshal(msg.Result)
	if err != nil {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	c, _ := b.loadCache()
	c.URL = b.url
	if kind == "tools" {
		c.Tools = result
	} else {
		c.Initialize = result
	}
	out, err := json.Marshal(c)
	if err != nil {
		return
	}
	writePrivate(b.cacheFile(), out)
}

// writePrivate writes data to file through a temporary file, readable by
// this user only.
func writePrivate(file string, data []byte) {
	dir := filepath.Dir(file)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Chmod(name, 0o600) != nil || os.Rename(name, file) != nil {
		_ = os.Remove(name) //nolint:errcheck // best effort
	}
}

// The offline marker tells this chat's hooks that CodeSynapse cannot be
// reached, so they skip the checks that only make sense with it: a guard that
// turns a Read back towards a tool that answers offlineText would block the
// chat. It holds a time and whether the chat was refused, nothing else.
type offlineMarker struct {
	At      time.Time `json:"at"`
	Refused bool      `json:"refused,omitempty"`
}

func offlineFile(session string) string {
	if execution.Validate(execution.Identity{TenantID: "t", SubjectID: "s", SessionID: session, ParentSessionID: session}) != nil {
		return ""
	}
	dir, err := bridgeCacheRoot()
	if err != nil {
		return ""
	}
	// The prefix keeps names such as "." and ".." inside the folder.
	return filepath.Join(dir, "codesynapse", "offline", "session-"+session)
}

func (b *bridge) writeMarker(refused bool) {
	if b.parent != nil || b.session == "" {
		return
	}
	file := offlineFile(b.session)
	if file == "" {
		return
	}
	if data, err := json.Marshal(offlineMarker{At: time.Now().UTC(), Refused: refused}); err == nil {
		writePrivate(file, data)
	}
}

func (b *bridge) clearMarker() {
	if b.parent != nil || b.session == "" {
		return
	}
	if file := offlineFile(b.session); file != "" {
		_ = os.Remove(file) //nolint:errcheck // absent is the goal
	}
}

// codesynapseOffline reports whether the chat's bridge recently found
// CodeSynapse unreachable or refused.
func codesynapseOffline(session string) bool {
	file := offlineFile(session)
	if file == "" {
		return false
	}
	data, err := os.ReadFile(file) //nolint:gosec // our own marker
	if err != nil || len(data) > 4096 {
		return false
	}
	var m offlineMarker
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	fresh := offlineFresh
	if m.Refused {
		fresh = refusedFresh
	}
	// A time ahead of the clock (a damaged file, a clock set back) must not
	// keep the guards off for good.
	age := time.Since(m.At)
	return age < fresh && age > -time.Minute
}
