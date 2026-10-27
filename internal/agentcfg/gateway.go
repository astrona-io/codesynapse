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
	"path/filepath"
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

func validGatewayBase(base string) error {
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

// writeSettings backs the old file up under a timestamped name, then writes.
func writeSettings(file string, doc map[string]any, old []byte, now time.Time) error {
	if old != nil {
		bak := fmt.Sprintf("%s.%s.bak", file, now.UTC().Format("20060102T150405Z"))
		if err := os.WriteFile(bak, old, 0o600); err != nil {
			return fmt.Errorf("backing up %s: %w", file, err)
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(file, append(out, '\n'), 0o600) //nolint:gosec // the user's own settings file
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
// with the key masked. The settings file is backed up first.
func SetClaudeGateway(file, base, key string, now time.Time) (string, error) {
	if !strings.HasPrefix(key, gatewayKeyPrefix) {
		return "", errors.New("the gateway key must be a CodeSynapse key (csk_…)")
	}
	if err := validGatewayBase(base); err != nil {
		return "", err
	}
	base = strings.TrimRight(base, "/")
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
	if err := writeSettings(file, doc, old, now); err != nil {
		return "", err
	}
	return strings.Join(diff, "\n"), nil
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
	doc, old, err := readSettings(file)
	if err != nil {
		return "", err
	}
	env, _ := doc["env"].(map[string]any)
	hdrs, _ := env[gatewayHeaderKey].(string)
	keep, ours := otherHeaders(hdrs)
	if !ours {
		return "nothing to remove: no CodeSynapse gateway header in " + file, nil
	}
	var diff []string
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
