package agentcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Claude Code reaches the gateway through two settings.json env entries. The
// key travels in its own header so the caller's Anthropic credential keeps
// Authorization / x-api-key.
const (
	GatewayKeyEnv    = "CODESYNAPSE_GATEWAY_KEY"
	gatewayBaseKey   = "ANTHROPIC_BASE_URL"
	gatewayHeaderKey = "ANTHROPIC_CUSTOM_HEADERS"
	gatewayHeader    = "X-CodeSynapse-Key"
	gatewayKeyPrefix = "csk_"
)

// CheckGateway asks the gateway whether it is on and accepts the key, before
// anything is written. GET /v1/models answers 200 or 503 (no local model) for
// a valid key; 404 means the gateway is off and 401 that the key is refused.
func CheckGateway(ctx context.Context, client *http.Client, base, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("gateway not reachable at %s: %w", base, err)
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusServiceUnavailable:
		return nil
	case http.StatusNotFound:
		return errors.New("the gateway is off: start the API with CODESYNAPSE_GATEWAY=true")
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.New("the gateway refused the key")
	}
	return fmt.Errorf("gateway answered %s", resp.Status)
}

// ValidGatewayBase accepts https, or http to this machine only, because the
// key travels in a header.
func ValidGatewayBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return fmt.Errorf("gateway URL %q is not a URL", base)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); u.Scheme == "http" && (host == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return errors.New("the gateway URL must be https, or http to this machine: the key travels in a header")
}

func readSettings(file string) (map[string]any, []byte, error) {
	doc := map[string]any{}
	data, err := os.ReadFile(file) //nolint:gosec // the user's own settings file
	switch {
	case errors.Is(err, os.ErrNotExist):
		return doc, nil, nil
	case err != nil:
		return nil, nil, err
	case len(strings.TrimSpace(string(data))) > 0:
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, nil, fmt.Errorf("%s is not plain JSON (comments?); set the gateway env by hand: %w", file, err)
		}
	}
	return doc, data, nil
}

// writeSettings backs the old file up under a timestamped name, then
// replaces it atomically, so Claude Code never reads half a file. Callers
// hold the settings lock (withFileLock) around the read and the write.
func writeSettings(file string, doc map[string]any, old []byte, now time.Time) error {
	if old != nil {
		bak := fmt.Sprintf("%s.%s.bak", file, now.UTC().Format("20060102T150405Z"))
		if err := WritePrivate(bak, old); err != nil {
			return fmt.Errorf("backing up %s: %w", file, err)
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivate(file, append(out, '\n'))
}

// headerLines splits ANTHROPIC_CUSTOM_HEADERS (one "Name: value" per line).
func headerLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func isOurHeader(l string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), strings.ToLower(gatewayHeader)+":")
}

// otherHeaders returns the header lines that are not ours, and whether ours was there.
func otherHeaders(s string) (keep []string, ours bool) {
	for _, l := range headerLines(s) {
		if isOurHeader(l) {
			ours = true
			continue
		}
		keep = append(keep, l)
	}
	return keep, ours
}

func mask(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:8] + "…"
}

// SetClaudeGateway points Claude Code at the gateway. It refuses to replace a
// base URL it did not write, keeps other custom headers, and returns a diff
// with the key masked. The settings file is backed up first. Proxy hooks
// SetClaudeGatewayProxy added are taken out (--failover=false).
func SetClaudeGateway(file, base, key string, now time.Time) (string, error) {
	return setClaudeGateway(file, base, key, "", now)
}

// SetClaudeGatewayProxy points Claude Code at the local failover proxy
// (proxyBase, http on loopback) and adds SessionStart and UserPromptSubmit
// hooks that run ensure (`codesynapse gateway proxy --ensure`), so the
// proxy is up before a session's first model request and again after it
// stopped for being idle.
func SetClaudeGatewayProxy(file, proxyBase, key, ensure string, now time.Time) (string, error) {
	if !ensureHook.MatchString(ensure) {
		return "", errors.New("the proxy hook must run `codesynapse gateway proxy --ensure`")
	}
	return setClaudeGateway(file, proxyBase, key, ensure, now)
}

func setClaudeGateway(file, base, key, ensure string, now time.Time) (string, error) {
	if !strings.HasPrefix(key, gatewayKeyPrefix) {
		return "", errors.New("the gateway key must be a CodeSynapse key (csk_…)")
	}
	if err := ValidGatewayBase(base); err != nil {
		return "", err
	}
	base = strings.TrimRight(base, "/")
	var diff string
	err := withFileLock(file, func() (err error) {
		diff, err = setClaudeGatewayLocked(file, base, key, ensure, now)
		return err
	})
	return diff, err
}

func setClaudeGatewayLocked(file, base, key, ensure string, now time.Time) (string, error) {
	doc, old, err := readSettings(file)
	if err != nil {
		return "", err
	}
	env, _ := doc["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	hdrs, _ := env[gatewayHeaderKey].(string)
	keep, ours := otherHeaders(hdrs)
	prev, _ := env[gatewayBaseKey].(string)
	if prev != "" && prev != base && !ours {
		return "", fmt.Errorf("%s already sets %s to %s; remove it first or point it at the gateway by hand", file, gatewayBaseKey, prev)
	}
	var diff []string
	switch {
	case prev == "":
		diff = append(diff, fmt.Sprintf("+ env.%s: %s", gatewayBaseKey, base))
	case prev != base:
		diff = append(diff, fmt.Sprintf("~ env.%s: %s -> %s", gatewayBaseKey, prev, base))
	}
	diff = append(diff, fmt.Sprintf("~ env.%s: %s: %s", gatewayHeaderKey, gatewayHeader, mask(key)))
	env[gatewayBaseKey] = base
	env[gatewayHeaderKey] = strings.Join(append(keep, gatewayHeader+": "+key), "\n")
	doc["env"] = env
	if removeEnsureHooks(doc) && ensure == "" {
		diff = append(diff, "- hooks: codesynapse gateway proxy --ensure")
	}
	if ensure != "" {
		addEnsureHooks(doc, ensure)
		diff = append(diff, "+ hooks.SessionStart, hooks.UserPromptSubmit: "+ensure)
	}
	if err := writeSettings(file, doc, old, now); err != nil {
		return "", err
	}
	return strings.Join(diff, "\n"), nil
}

// ensureHook marks the hooks SetClaudeGatewayProxy added (the binary path is
// shell-quoted: '/path/codesynapse' gateway proxy --ensure).
var ensureHook = regexp.MustCompile(`codesynapse'? gateway proxy --ensure$`)

// ensureEvents run ensure: at the start of a session, and before each prompt
// so a session left open past the proxy's idle time finds it running again.
var ensureEvents = []string{"SessionStart", "UserPromptSubmit"}

// ensureTimeout (seconds) bounds the hook; ensure returns within a few.
const ensureTimeout = 10

func addEnsureHooks(doc map[string]any, cmd string) {
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, event := range ensureEvents {
		groups, _ := hooks[event].([]any)
		hooks[event] = append(groups, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": ensureTimeout}},
		})
	}
	doc["hooks"] = hooks
}

// removeEnsureHooks takes out every hook group that runs ensure and says
// whether there was one. Other hooks stay.
func removeEnsureHooks(doc map[string]any) bool {
	hooks, _ := doc["hooks"].(map[string]any)
	removed := false
	for event, v := range hooks {
		groups, _ := v.([]any)
		var kept []any
		for _, g := range groups {
			if isEnsureGroup(g) {
				removed = true
				continue
			}
			kept = append(kept, g)
		}
		switch {
		case len(kept) == 0 && len(groups) > 0:
			delete(hooks, event)
		case len(kept) != len(groups):
			hooks[event] = kept
		}
	}
	if removed && len(hooks) == 0 {
		delete(doc, "hooks")
	}
	return removed
}

func isEnsureGroup(group any) bool {
	g, _ := group.(map[string]any)
	hs, _ := g["hooks"].([]any)
	for _, h := range hs {
		m, _ := h.(map[string]any)
		if c, _ := m["command"].(string); ensureHook.MatchString(c) {
			return true
		}
	}
	return false
}

// PointClaudeGateway switches ANTHROPIC_BASE_URL between the proxy and the
// gateway it fronts, without a backup (only our own value changes). The
// ensure hook falls back to the gateway when the proxy cannot run, so Claude
// Code is never left on a dead port, and returns to the proxy once it runs
// again. Nothing changes unless the CodeSynapse header is present and the
// current value is one of the two, so a base URL set by hand stays.
func PointClaudeGateway(file, proxyBase, gatewayBase, to string) (bool, error) {
	if to != proxyBase && to != gatewayBase {
		return false, errors.New("the base URL can only switch between the proxy and its gateway")
	}
	changed := false
	err := withFileLock(file, func() error {
		doc, _, err := readSettings(file)
		if err != nil {
			return err
		}
		env, _ := doc["env"].(map[string]any)
		hdrs, _ := env[gatewayHeaderKey].(string)
		if _, ours := otherHeaders(hdrs); !ours {
			return nil
		}
		cur, _ := env[gatewayBaseKey].(string)
		if cur == to || (cur != proxyBase && cur != gatewayBase) {
			return nil
		}
		env[gatewayBaseKey] = to
		changed = true
		return writeSettings(file, doc, nil, time.Time{})
	})
	return changed && err == nil, err
}

// ClaudeGatewayConfig returns the base URL and key SetClaudeGateway wrote to
// the settings file. ok is false unless the CodeSynapse header is present, so
// a base URL somebody else set is never ours to check.
func ClaudeGatewayConfig(file string) (base, key string, ok bool) {
	doc, _, err := readSettings(file)
	if err != nil {
		return "", "", false
	}
	env, _ := doc["env"].(map[string]any)
	base, _ = env[gatewayBaseKey].(string)
	hdrs, _ := env[gatewayHeaderKey].(string)
	for _, l := range headerLines(hdrs) {
		if isOurHeader(l) {
			_, key, _ = strings.Cut(l, ":")
			key = strings.TrimSpace(key)
		}
	}
	return base, key, base != "" && key != ""
}

// RemoveClaudeGateway undoes SetClaudeGateway. The base URL goes only when
// the CodeSynapse header marks it as ours; other headers stay.
func RemoveClaudeGateway(file string, now time.Time) (string, error) {
	var diff string
	err := withFileLock(file, func() (err error) {
		diff, err = removeClaudeGatewayLocked(file, now)
		return err
	})
	return diff, err
}

func removeClaudeGatewayLocked(file string, now time.Time) (string, error) {
	doc, old, err := readSettings(file)
	if err != nil {
		return "", err
	}
	env, _ := doc["env"].(map[string]any)
	hdrs, _ := env[gatewayHeaderKey].(string)
	keep, ours := otherHeaders(hdrs)
	var diff []string
	if removeEnsureHooks(doc) {
		diff = append(diff, "- hooks: codesynapse gateway proxy --ensure")
	}
	if !ours {
		if len(diff) == 0 {
			return "nothing to remove: no CodeSynapse gateway header in " + file, nil
		}
		if err := writeSettings(file, doc, old, now); err != nil {
			return "", err
		}
		return strings.Join(diff, "\n"), nil
	}
	if base, ok := env[gatewayBaseKey].(string); ok {
		diff = append(diff, fmt.Sprintf("- env.%s: %s", gatewayBaseKey, base))
		delete(env, gatewayBaseKey)
	}
	diff = append(diff, fmt.Sprintf("- env.%s: %s", gatewayHeaderKey, gatewayHeader))
	if len(keep) > 0 {
		env[gatewayHeaderKey] = strings.Join(keep, "\n")
	} else {
		delete(env, gatewayHeaderKey)
	}
	if len(env) == 0 {
		delete(doc, "env")
	}
	if err := writeSettings(file, doc, old, now); err != nil {
		return "", err
	}
	return strings.Join(diff, "\n"), nil
}
