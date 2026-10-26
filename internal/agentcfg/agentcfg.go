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

// personalKeyPrefix mirrors accounts.KeyPrefix without pulling in the accounts package.
const personalKeyPrefix = "csk_"

// Runtime variables must be supplied by the launcher for one chat thread.
const (
	SessionEnv       = "CODESYNAPSE_SESSION_ID"
	ParentSessionEnv = "CODESYNAPSE_PARENT_SESSION_ID"
)

// Server describes the MCP endpoint to register.
type Server struct {
	URL   string
	Token string
	// Command is the codesynapse binary Claude Code starts as a per-chat
	// bridge, and APIURL the API it registers the chat with.
	Command string
	APIURL  string
	// StoreToken keeps the key in the user's config folder for the bridge,
	// so Claude Code's settings never hold it.
	StoreToken func(token string) (string, error)
	// ContextCap, in tokens, is where Claude Code and Codex compact (0
	// leaves their settings alone). An existing setting is kept unless
	// ForceContextCap: the user asked for this cap.
	ContextCap      int
	ForceContextCap bool
	// Note reports what an installer changed besides the connection.
	Note func(msg string)
}

func (s Server) note(msg string) {
	if s.Note != nil {
		s.Note(msg)
	}
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
		t := Target{
			ID: "claude", Description: "Claude Code (user scope, all projects; one bridge per chat)",
			install: func(ctx context.Context, s Server) error {
				if err := installClaude(ctx, s); err != nil {
					return err
				}
				return setClaudeCap(home, s)
			},
		}
		if ClaudePluginEnabled(home, cwd) {
			t.Description = "Claude Code (plugin found: sets the context cap and removes the older setup it replaces)"
			t.install = func(ctx context.Context, s Server) error {
				if err := cleanClaudeForPlugin(ctx, home, cwd, s); err != nil {
					return err
				}
				return setClaudeCap(home, s)
			}
		}
		out = append(out, t)
	}
	if _, err := exec.LookPath("codex"); err == nil {
		p := filepath.Join(home, ".codex", "hooks.json")
		out = append(out, Target{
			ID: "codex", Description: "Codex (MCP connection and all lifecycle hooks; review with /hooks)",
			install: func(ctx context.Context, s Server) error {
				if err := prepareLifecycle(s); err != nil {
					return err
				}
				if err := installCodex(ctx, s); err != nil {
					return err
				}
				if s.ContextCap > 0 {
					msg, err := SetCodexContextCap(filepath.Join(home, ".codex", "config.toml"), s.ContextCap, s.ForceContextCap)
					if err != nil {
						return err
					}
					s.note(msg)
				}
				return InstallLifecycleHooks(p, "codex", s.Command, s.APIURL)
			},
		})
	}
	if dirExists(filepath.Join(home, ".gemini")) {
		p := filepath.Join(home, ".gemini", "settings.json")
		out = append(out, Target{
			ID: "gemini", Description: "Gemini CLI (~/.gemini/settings.json)", Path: p,
			install: func(_ context.Context, s Server) error {
				return mergeJSON(p, []string{"mcpServers", Name}, map[string]any{
					"httpUrl": s.URL,
					"headers": runtimeHeaders("Bearer $"+TokenEnv, "$"+SessionEnv, "$"+ParentSessionEnv),
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
					"headers": runtimeHeaders("Bearer {env:"+TokenEnv+"}", "{env:"+SessionEnv+"}", "{env:"+ParentSessionEnv+"}"),
				})
			},
		})
	}
	if dirExists(filepath.Join(home, ".cursor")) {
		p := filepath.Join(home, ".cursor", "mcp.json")
		out = append(out, Target{
			ID: "cursor", Description: "Cursor (MCP connection and all lifecycle hooks)", Path: p,
			install: func(_ context.Context, s Server) error {
				if err := prepareLifecycle(s); err != nil {
					return err
				}
				if err := mergeJSON(p, []string{"mcpServers", Name}, map[string]any{
					"type": "stdio", "command": s.Command, "args": []string{"mcp", "bridge", "--client", "cursor", "--url", s.URL, "--api-url", s.APIURL},
				}); err != nil {
					return err
				}
				return InstallLifecycleHooks(filepath.Join(home, ".cursor", "hooks.json"), "cursor", s.Command, s.APIURL)
			},
		})
	}
	p := filepath.Join(cwd, ".mcp.json")
	out = append(out, Target{
		ID: "project", Description: "This project's .mcp.json (shared with the repo; any MCP client)", Path: p,
		install: func(_ context.Context, s Server) error {
			// Shared with the repository, so the bridge is found on PATH,
			// not at this machine's install path; the key stays in the
			// user's config folder.
			if s.Token != "" && s.StoreToken != nil {
				if _, err := s.StoreToken(s.Token); err != nil {
					return err
				}
			}
			return mergeJSON(p, []string{"mcpServers", Name}, map[string]any{
				"type": "stdio", "command": "codesynapse", "args": []string{"mcp", "bridge", "--url", s.URL, "--api-url", s.APIURL},
			})
		},
	})
	return out
}

func prepareLifecycle(s Server) error {
	if !strings.HasPrefix(s.Token, personalKeyPrefix) || s.StoreToken == nil || s.Command == "" {
		return errors.New("lifecycle hooks need an installed binary and a personal API key (csk_…) in " + TokenEnv)
	}
	_, err := s.StoreToken(s.Token)
	return err
}

func runtimeHeaders(authorization, primary, parent string) map[string]any {
	return map[string]any{"Authorization": authorization, "X-CodeSynapse-Session": primary, "X-CodeSynapse-Parent-Session": parent}
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

// installClaude registers `codesynapse mcp bridge` as a stdio server. Claude
// Code gives stdio servers the chat's ID (CLAUDE_CODE_SESSION_ID) but not
// remote servers or their header helpers, and the MCP server needs that ID
// in its runtime headers.
func installClaude(ctx context.Context, s Server) error {
	if s.Token == "" {
		return errors.New("the bridge needs your personal key; set " + TokenEnv)
	}
	if !strings.HasPrefix(s.Token, personalKeyPrefix) {
		// The .env tokens are installation tokens; session registration refuses them.
		return fmt.Errorf("%s is not a personal key (%s…); create one in the dashboard under Settings → API keys", TokenEnv, personalKeyPrefix)
	}
	if s.Command == "" || s.StoreToken == nil {
		return errors.New("no codesynapse binary to start as the bridge")
	}
	if _, err := s.StoreToken(s.Token); err != nil {
		return err
	}
	cfg, err := json.Marshal(map[string]any{
		"type": "stdio", "command": s.Command,
		"args": []string{"mcp", "bridge", "--url", s.URL, "--api-url", s.APIURL},
	})
	if err != nil {
		return err
	}
	_ = run(ctx, "claude", "mcp", "remove", Name, "--scope", "user") //nolint:errcheck // absent is fine
	return run(ctx, "claude", "mcp", "add-json", Name, string(cfg), "--scope", "user")
}

func setClaudeCap(home string, s Server) error {
	if s.ContextCap == 0 {
		return nil
	}
	msg, err := SetClaudeContextCap(filepath.Join(home, ".claude", "settings.json"), s.ContextCap, s.ForceContextCap)
	if err != nil {
		return err
	}
	s.note(msg)
	return nil
}

// cleanClaudeForPlugin removes what an earlier `mcp install` or `mcp hooks`
// set up and the plugin now brings itself: the user-scope server and the
// hooks in the user's and this project's private settings. The project's
// shared settings.json is only reported, since teammates may lack the plugin.
func cleanClaudeForPlugin(ctx context.Context, home, cwd string, s Server) error {
	if strings.HasPrefix(s.Token, personalKeyPrefix) && s.StoreToken != nil {
		// The plugin's bridge reads the key from the same file.
		if _, err := s.StoreToken(s.Token); err != nil {
			return err
		}
	}
	if claudeUserServer(home) {
		if err := run(ctx, "claude", "mcp", "remove", Name, "--scope", "user"); err != nil {
			return err
		}
		s.note("removed the user-scope " + Name + " server; the plugin's bridge replaces it")
	}
	for _, f := range []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json")} {
		n, err := RemoveClaudeHooks(f)
		if err != nil {
			return err
		}
		if n > 0 {
			s.note(fmt.Sprintf("removed %d CodeSynapse hook(s) from %s; the plugin runs them", n, f))
		}
	}
	if hasClaudeHooks(filepath.Join(cwd, ".claude", "settings.json")) {
		s.note("the project's .claude/settings.json (shared) also has CodeSynapse hooks; remove them there once the team uses the plugin")
	}
	return nil
}

func installCodex(ctx context.Context, s Server) error {
	_ = run(ctx, "codex", "mcp", "remove", Name) //nolint:errcheck // absent is fine
	return run(ctx, "codex", "mcp", "add", Name, "--", s.Command, "mcp", "bridge", "--client", "codex", "--url", s.URL, "--api-url", s.APIURL)
}

// mergeJSON sets value at path inside the JSON object stored in file,
// keeping every other key. The original is copied to file+".bak" first.
// Files with comments (JSONC) are refused rather than rewritten lossily.
// An empty path writes the complete, already-merged root object.
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
	if len(path) == 0 {
		root = value.(map[string]any)
	} else {
		for _, key := range path[:len(path)-1] {
			child, ok := node[key].(map[string]any)
			if !ok {
				child = map[string]any{}
				node[key] = child
			}
			node = child
		}
		node[path[len(path)-1]] = value
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(file, append(out, '\n'), 0o600)
}
