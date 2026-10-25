package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/astrona-io/codesynapse/internal/forge"
	"github.com/astrona-io/codesynapse/internal/gitx"
)

// The server cannot see the developer's checkout, so a tool call without
// "repo" only works when exactly one repository is indexed. The bridge runs
// next to the checkout: when a call to a tool that takes "repo" names none, it
// fills in the repository this checkout is (the indexed repository with the
// same origin remote, else the one named like the directory, else the name the
// remote gives). An explicit repo is never replaced, and a checkout that is
// not an indexed repository leaves the call as it was, so the server answers
// as before. The name only selects what the caller may already read: the
// server checks access on every call.

// learnRepoTools notes which tools take a repo argument from a tools/list
// answer, so the bridge never adds an argument a tool does not declare.
func (b *bridge) learnRepoTools(data []byte) {
	var r struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &r) != nil || len(r.Result.Tools) == 0 {
		return
	}
	known := map[string]bool{}
	for _, t := range r.Result.Tools {
		if _, ok := t.InputSchema.Properties["repo"]; ok {
			known[t.Name] = true
		}
	}
	b.mu.Lock()
	b.repoTools = known
	b.mu.Unlock()
}

// withRepo returns msg with this checkout's repository as the repo argument
// when msg is a tools/call to a tool that takes one and names none.
func (b *bridge) withRepo(ctx context.Context, msg []byte, m rpcMessage) []byte {
	if m.Method != "tools/call" {
		return msg
	}
	var c toolCall
	if json.Unmarshal(msg, &c) != nil {
		return msg
	}
	b.mu.Lock()
	takes := b.repoTools[c.Params.Name]
	b.mu.Unlock()
	if !takes {
		return msg
	}
	args := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(c.Params.Arguments)) > 0 && json.Unmarshal(c.Params.Arguments, &args) != nil {
		return msg
	}
	if raw, ok := args["repo"]; ok {
		var s string
		if json.Unmarshal(raw, &s) != nil || s != "" {
			return msg // named, or not a string: the server decides
		}
	}
	repo := b.checkoutRepo(ctx)
	if repo == "" {
		return msg
	}
	name, err := json.Marshal(repo)
	if err != nil {
		return msg
	}
	args["repo"] = name
	var top, params map[string]json.RawMessage
	if json.Unmarshal(msg, &top) != nil || json.Unmarshal(top["params"], &params) != nil {
		return msg
	}
	if params["arguments"], err = json.Marshal(args); err != nil {
		return msg
	}
	if top["params"], err = json.Marshal(params); err != nil {
		return msg
	}
	out, err := json.Marshal(top)
	if err != nil {
		return msg
	}
	return out
}

// checkoutRepo is the repository name of the checkout the bridge runs in, ""
// when there is none. It is worked out once.
func (b *bridge) checkoutRepo(ctx context.Context) string {
	b.repoOnce.Do(func() {
		dir, err := b.checkout(ctx)
		if err != nil {
			return
		}
		if b.report.base != "" {
			if name, err := b.report.resolveRepo(ctx, dir); err == nil {
				b.repoName = name
				return
			}
		}
		b.repoName = forge.NameFromRemote(gitx.HTTPSRemote(gitx.OriginURL(ctx, dir)))
		b.repoName = strings.TrimSpace(b.repoName)
	})
	return b.repoName
}
