// Package agentcfg registers the CodeSynapse MCP server with local AI coding
// agents (Claude Code, Codex, Gemini CLI, OpenCode, Cursor, and a project
// .mcp.json). Where an agent supports it, configs reference the token through
// an environment variable instead of storing the secret.
package agentcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Name is the server name registered with every agent.
const Name = "codesynapse"

// TokenEnv is the environment variable agents read the bearer token from.
const TokenEnv = "CODESYNAPSE_MCP_TOKEN" //nolint:gosec // env var name

// Server describes the MCP endpoint to register.
type Server struct {
	URL   string
	Token string
}

// Target is one agent configuration that can be installed.
type Target struct {
	ID          string
	Description string
	// Path is the file written, or "" when the agent's own CLI is used.
	Path string
	// StoresSecret is true when the token ends up in the agent's config.
	StoresSecret bool
	install      func(ctx context.Context, s Server) error
}

// Install registers s with the target.
func (t Target) Install(ctx context.Context, s Server) error { return t.install(ctx, s) }

// Detect returns targets whose agent is present on this machine. home is
// the user's home directory and cwd the project directory.
func Detect(home, cwd string) []Target {
	var out []Target
	if _, err := exec.LookPath("claude"); err == nil {
		out = append(out, Target{
			ID: "claude", Description: "Claude Code (user scope, all projects)", StoresSecret: true,
			install: installClaude,
		})
	}
	if _, err := exec.LookPath("codex"); err == nil {
		out = append(out, Target{
			ID: "codex", Description: "Codex CLI (~/.codex/config.toml)",
			install: installCodex,
		})
	}
	if dirExists(filepath.Join(home, ".gemini")) {
		p := filepath.Join(home, ".gemini", "settings.json")
		out = append(out, Target{
			ID: "gemini", Description: "Gemini CLI (~/.gemini/settings.json)", Path: p,
			install: func(_ context.Context, s Server) error {
				return mergeJSON(p, []string{"mcpServers", Name}, map[string]any{
					"httpUrl": s.URL,
					"headers": map[string]any{"Authorization": "Bearer $" + TokenEnv},
				})
			},
		})
	}
	if dirExists(filepath.Join(home, ".config", "opencode")) {
		p := filepath.Join(home, ".config", "opencode", "opencode.json")
		out = append(out, Target{
			ID: "opencode", Description: "OpenCode (~/.config/opencode/opencode.json)", Path: p,
			install: func(_ context.Context, s Server) error {
				return mergeJSON(p, []string{"mcp", Name}, map[string]any{
					"type": "remote", "url": s.URL, "enabled": true,
					"headers": map[string]any{"Authorization": "Bearer {env:" + TokenEnv + "}"},
				})
			},
		})
	}
	if dirExists(filepath.Join(home, ".cursor")) {
		p := filepath.Join(home, ".cursor", "mcp.json")
		out = append(out, Target{
			ID: "cursor", Description: "Cursor (~/.cursor/mcp.json)", Path: p,
			install: func(_ context.Context, s Server) error {
				return mergeJSON(p, []string{"mcpServers", Name}, map[string]any{
					"url":     s.URL,
					"headers": map[string]any{"Authorization": "Bearer ${env:" + TokenEnv + "}"},
				})
			},
		})
	}
	p := filepath.Join(cwd, ".mcp.json")
	out = append(out, Target{
		ID: "project", Description: "This project's .mcp.json (shared with the repo; any MCP client)", Path: p,
		install: func(_ context.Context, s Server) error {
			return mergeJSON(p, []string{"mcpServers", Name}, map[string]any{
				"type": "http", "url": s.URL,
				"headers": map[string]any{"Authorization": "Bearer ${" + TokenEnv + "}"},
			})
		},
	})
	return out
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed agent binaries, explicit argv
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func installClaude(ctx context.Context, s Server) error {
	if s.Token == "" {
		return errors.New("claude needs the token itself; set " + TokenEnv)
	}
	cfg, err := json.Marshal(map[string]any{
		"type": "http", "url": s.URL,
		"headers": map[string]string{"Authorization": "Bearer " + s.Token},
	})
	if err != nil {
		return err
	}
	_ = run(ctx, "claude", "mcp", "remove", Name, "--scope", "user") //nolint:errcheck // absent is fine
	return run(ctx, "claude", "mcp", "add-json", Name, string(cfg), "--scope", "user")
}

func installCodex(ctx context.Context, s Server) error {
	_ = run(ctx, "codex", "mcp", "remove", Name) //nolint:errcheck // absent is fine
	return run(ctx, "codex", "mcp", "add", Name, "--url", s.URL, "--bearer-token-env-var", TokenEnv)
}

// mergeJSON sets value at path inside the JSON object stored in file,
// keeping every other key. The original is copied to file+".bak" first.
// Files with comments (JSONC) are refused rather than rewritten lossily.
func mergeJSON(file string, path []string, value any) error {
	root := map[string]any{}
	data, err := os.ReadFile(file) //nolint:gosec // path chosen by Detect
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return fmt.Errorf("%s is not plain JSON (comments?); add the server manually: %w", file, err)
			}
		}
		//nolint:gosec // file comes from Detect: fixed paths under home or cwd
		if err := os.WriteFile(file+".bak", data, 0o600); err != nil {
			return fmt.Errorf("backing up %s: %w", file, err)
		}
	}
	node := root
	for _, key := range path[:len(path)-1] {
		child, ok := node[key].(map[string]any)
		if !ok {
			child = map[string]any{}
			node[key] = child
		}
		node = child
	}
	node[path[len(path)-1]] = value
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(file, append(out, '\n'), 0o600)
}
