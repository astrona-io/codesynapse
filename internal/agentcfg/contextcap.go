package agentcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The context cap is the chat size, in tokens, at which an agent compacts.
// Agents with a million-token window otherwise carry nearly a million tokens
// into every prompt; with the memory hooks the older turns are offloaded
// before each compaction, so a far smaller cap loses little.
const (
	DefaultContextCap = 150_000
	// Claude Code accepts an auto-compact window of 100k to 1M tokens.
	MinContextCap = 100_000
	MaxContextCap = 1_000_000

	claudeCapKey = "autoCompactWindow"
	codexCapKey  = "model_auto_compact_token_limit"
)

// ParseContextCap reads 150000, 150k or 1M. "0" and "off" give 0: leave the
// agents' own settings alone.
func ParseContextCap(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "0" || s == "off" {
		return 0, nil
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1_000, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "m")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > MaxContextCap/mult || n*mult < MinContextCap {
		return 0, fmt.Errorf("context cap %q: use 100k to 1M tokens, or 0 to leave it unchanged", s)
	}
	return n * mult, nil
}

// SetClaudeContextCap sets autoCompactWindow in a Claude Code settings file.
// An existing value is kept unless force (the user asked for a cap). The
// percentage variable is removed: it applies to this window, so 30 would
// compact at 30% of the cap. It returns what it did, for the user.
func SetClaudeContextCap(file string, tokens int, force bool) (string, error) {
	doc := map[string]any{}
	data, err := os.ReadFile(file) //nolint:gosec // the user's own settings file
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", err
	case len(strings.TrimSpace(string(data))) > 0:
		if err := json.Unmarshal(data, &doc); err != nil {
			return "", fmt.Errorf("%s is not plain JSON (comments?); set %s by hand: %w", file, claudeCapKey, err)
		}
	}
	if old, ok := doc[claudeCapKey]; ok && !force {
		return fmt.Sprintf("kept your %s %v in %s", claudeCapKey, old, file), nil
	}
	doc[claudeCapKey] = tokens
	msg := fmt.Sprintf("Claude Code compacts at %s tokens (%s in %s)", capLabel(tokens), claudeCapKey, file)
	if env, ok := doc["env"].(map[string]any); ok {
		if _, ok := env[CompactEnv]; ok {
			delete(env, CompactEnv)
			msg += "; removed " + CompactEnv + ", which would apply on top of it"
		}
	}
	if err := mergeJSON(file, nil, doc); err != nil {
		return "", err
	}
	return msg, nil
}

// codexCapLine matches the setting at the top level of a Codex config.toml.
var codexCapLine = regexp.MustCompile(`^\s*` + codexCapKey + `\s*=`)

// SetCodexContextCap sets model_auto_compact_token_limit in a Codex
// config.toml. It edits only the top-level table (before the first [header])
// and keeps every other line; an existing value is kept unless force.
func SetCodexContextCap(file string, tokens int, force bool) (string, error) {
	data, err := os.ReadFile(file) //nolint:gosec // the user's own config file
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	line := fmt.Sprintf("%s = %d", codexCapKey, tokens)
	lines := strings.Split(string(data), "\n")
	at := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			break
		}
		if codexCapLine.MatchString(l) {
			at = i
			break
		}
	}
	switch {
	case at >= 0 && !force:
		return fmt.Sprintf("kept your %s in %s", strings.TrimSpace(lines[at]), file), nil
	case at >= 0:
		lines[at] = line
	case len(data) == 0:
		lines = []string{line, ""}
	default:
		lines = append([]string{line}, lines...)
	}
	if len(data) > 0 {
		if err := os.WriteFile(file+".bak", data, 0o600); err != nil { //nolint:gosec // the user's own config file, chosen by the CLI
			return "", fmt.Errorf("backing up %s: %w", file, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o600); err != nil { //nolint:gosec // the user's own config file, chosen by the CLI
		return "", err
	}
	return fmt.Sprintf("Codex compacts at %s tokens (%s in %s)", capLabel(tokens), codexCapKey, file), nil
}

func capLabel(tokens int) string {
	if tokens%1_000_000 == 0 {
		return strconv.Itoa(tokens/1_000_000) + "M"
	}
	if tokens%1_000 == 0 {
		return strconv.Itoa(tokens/1_000) + "k"
	}
	return strconv.Itoa(tokens)
}
