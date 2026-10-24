package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/coretools"
	"github.com/astrona-io/codesynapse/internal/forge"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
)

// Working-tree answers (docs/PLAN.md, agent workflow roadmap step 4). The
// server's graph reflects the last sync, so in the middle of a task, when
// the agent relies on it most, it misses the files the agent just edited.
// While the checkout has changes the server cannot have (uncommitted edits,
// or commits not pushed yet), the bridge answers the core repository tools
// (coretools.Local) for this checkout from a graph of the working tree,
// built on this machine like `review --offline`: nothing new leaves it.
// Every other call, and every call while the tree is clean, goes to the
// server as before. Local answers say so in _meta and in a leading note.

// worktreeBuildTimeout bounds how long a call waits for the local graph.
const worktreeBuildTimeout = 2 * time.Minute

type worktree struct {
	root, repo string
	logger     *slog.Logger

	mu      sync.Mutex
	key     string       // fingerprint of the tree the graph was built from
	graph   store.Reader // nil until the first build
	pending *worktreeBuild

	// The base a change is judged against (get_pr_context): one graph per
	// base commit, built once.
	baseMu     sync.Mutex
	baseCommit string
	baseGraph  store.Reader
}

type worktreeBuild struct {
	key   string
	done  chan struct{}
	graph store.Reader
	err   error
}

// newWorktree finds the checkout around dir and the name the server knows
// it by (repo, or the origin remote's). It returns nil when either is
// unknown, which leaves the bridge forwarding everything.
func newWorktree(ctx context.Context, dir, repo string, logger *slog.Logger) *worktree {
	out, err := gitx.Run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	}
	root := strings.TrimSpace(string(out))
	if repo == "" {
		repo = forge.NameFromRemote(gitx.HTTPSRemote(gitx.OriginURL(ctx, root)))
	}
	if root == "" || repo == "" {
		return nil
	}
	return &worktree{root: root, repo: repo, logger: logger}
}

// changes fingerprints what the server cannot have: uncommitted and
// untracked files (with their size and modification time) and commits
// ahead of the upstream branch. It returns "" for a clean tree.
func (w *worktree) changes(ctx context.Context) (key string, files int, err error) {
	status, err := gitx.Run(ctx, w.root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return "", 0, err
	}
	ahead := 0
	if out, err := gitx.Run(ctx, w.root, "rev-list", "--count", "@{upstream}..HEAD"); err == nil {
		ahead, _ = strconv.Atoi(strings.TrimSpace(string(out))) //nolint:errcheck // not a number means none
	}
	if len(status) == 0 && ahead == 0 {
		return "", 0, nil
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%d\x00", gitx.HeadCommit(ctx, w.root), ahead) //nolint:errcheck // hashes never fail
	h.Write(status)                                                            //nolint:errcheck // hashes never fail
	for entry := range strings.SplitSeq(string(status), "\x00") {
		if len(entry) < 4 {
			continue
		}
		files++
		if st, err := os.Stat(filepath.Join(w.root, filepath.FromSlash(entry[3:]))); err == nil {
			_, _ = fmt.Fprintf(h, "%s\x00%d\x00%d\x00", entry[3:], st.Size(), st.ModTime().UnixNano()) //nolint:errcheck // hashes never fail
		}
	}
	return hex.EncodeToString(h.Sum(nil)), files, nil
}

// graphFor returns the working-tree graph for key, building it once per
// key; calls for the same key share one build.
func (w *worktree) graphFor(ctx context.Context, key string) (store.Reader, error) {
	w.mu.Lock()
	if w.key == key && w.graph != nil {
		g := w.graph
		w.mu.Unlock()
		return g, nil
	}
	b := w.pending
	if b == nil || b.key != key {
		b = &worktreeBuild{key: key, done: make(chan struct{})}
		w.pending = b
		go w.build(context.WithoutCancel(ctx), b)
	}
	w.mu.Unlock()
	select {
	case <-b.done:
		return b.graph, b.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (w *worktree) build(ctx context.Context, b *worktreeBuild) {
	defer close(b.done)
	ctx, cancel := context.WithTimeout(ctx, worktreeBuildTimeout)
	defer cancel()
	// Ingest logs through the bridge's logger: stdout carries the protocol.
	ctx = logx.WithLogger(ctx, w.logger)
	start := time.Now()
	g, err := ingest.Run(ctx, ingest.Options{Repo: w.repo, Root: w.root})
	if err == nil {
		mem := memory.New()
		if err = mem.ReplaceRepo(ctx, w.repo, g); err == nil {
			b.graph = mem
		}
	}
	b.err = err
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == b {
		w.pending = nil
	}
	if err != nil {
		w.logger.Warn("working-tree graph failed; answering from the server", "error", err)
		return
	}
	w.key, w.graph = b.key, b.graph
	w.logger.Info("working-tree graph built", "repo", w.repo, "duration", time.Since(start).Round(time.Millisecond))
}

// toolCall is the part of a tools/call request the bridge looks at.
type toolCall struct {
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

// local reports whether a tools/call can be answered from the working
// tree: a core repository tool for this checkout (or no repository named).
func (w *worktree) local(msg []byte) (toolCall, bool) {
	var c toolCall
	if w == nil || json.Unmarshal(msg, &c) != nil || !slices.Contains(coretools.Local, c.Params.Name) {
		return c, false
	}
	var arg coretools.RepoArg
	if len(c.Params.Arguments) > 0 && json.Unmarshal(c.Params.Arguments, &arg) != nil {
		return c, false
	}
	return c, arg.Repo == "" || arg.Repo == w.repo
}

// answer runs a core tool against the working-tree graph and returns the
// tool result, or an error when the call should go to the server instead.
func (w *worktree) answer(ctx context.Context, c toolCall) (map[string]any, error) {
	key, files, err := w.changes(ctx)
	if err != nil || key == "" {
		return nil, errNoChanges
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeBuildTimeout)
	defer cancel()
	g, err := w.graphFor(ctx, key)
	if err != nil {
		return nil, err
	}
	out, err := w.runCoreTool(ctx, g, c.Params.Name, c.Params.Arguments)
	if err != nil {
		// The tool itself failed, as it would on the server: tell the agent.
		return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}}, nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	note := fmt.Sprintf("Answered from your working tree of %s (%d changed file(s) the CodeSynapse server has not indexed yet), not from the synced graph.", w.repo, files)
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": note}, {"type": "text", "text": string(raw)}},
		"structuredContent": json.RawMessage(raw),
		"_meta":             map[string]any{"codesynapse/source": "worktree", "codesynapse/changed_files": files},
	}, nil
}

var errNoChanges = errors.New("the working tree has no changes the server lacks")

// base returns the commit the working tree's change is judged against
// (where it left the upstream branch, else HEAD) and a graph of it, built
// once per commit in a temporary worktree. A nil graph means no comparison.
func (w *worktree) base(ctx context.Context) (string, store.Reader) {
	commit := w.baseCommitOf(ctx)
	if commit == "" {
		return "", nil
	}
	w.baseMu.Lock()
	defer w.baseMu.Unlock()
	if w.baseCommit == commit {
		return commit, w.baseGraph
	}
	ctx = logx.WithLogger(ctx, w.logger)
	g, err := ingest.AtCommit(ctx, w.root, commit, ingest.Options{Repo: w.repo})
	if err != nil {
		w.logger.Warn("base graph failed; get_pr_context answers without new and fixed findings", "commit", commit, "error", err)
		return commit, nil
	}
	mem := memory.New()
	if err := mem.ReplaceRepo(ctx, w.repo, g); err != nil {
		return commit, nil
	}
	w.baseCommit, w.baseGraph = commit, mem
	return commit, mem
}

// baseCommitOf is where the branch left its upstream, else HEAD.
func (w *worktree) baseCommitOf(ctx context.Context) string {
	if out, err := gitx.Run(ctx, w.root, "merge-base", "HEAD", "@{upstream}"); err == nil {
		return strings.TrimSpace(string(out))
	}
	return gitx.HeadCommit(ctx, w.root)
}

// currentChange fills a get_pr_context call that names no change with the
// working tree's change against base: its diff, or, when there are new
// untracked files a diff leaves out, the changed and new files.
func (w *worktree) currentChange(ctx context.Context, base string, in *coretools.PRContextIn) error {
	untracked, err := gitx.Run(ctx, w.root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	if len(untracked) == 0 {
		diff, err := gitx.WorkingDiff(ctx, w.root, base)
		if err != nil {
			return err
		}
		if len(diff) > 0 && len(diff) <= coretools.MaxDiffBytes {
			in.Diff = string(diff)
			return nil
		}
	}
	names, err := gitx.ChangedFiles(ctx, w.root, base)
	if err != nil {
		return err
	}
	for _, list := range [][]byte{names, untracked} {
		for f := range strings.SplitSeq(string(list), "\x00") {
			if f != "" && !slices.Contains(in.Files, f) {
				in.Files = append(in.Files, f)
			}
		}
	}
	in.Files = in.Files[:min(len(in.Files), 100)]
	return nil
}

// runCoreTool decodes a core tool's arguments and answers it from r.
func (w *worktree) runCoreTool(ctx context.Context, r store.Reader, name string, args json.RawMessage) (any, error) {
	repo := w.repo
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	decode := func(v any) error {
		if err := json.Unmarshal(args, v); err != nil {
			return fmt.Errorf("invalid arguments for %s: %w", name, err)
		}
		return nil
	}
	switch name {
	case "minimal_context":
		var in coretools.MinimalContextIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return coretools.MinimalContext(ctx, r, repo, in)
	case "get_symbol_context":
		var in coretools.SymbolContextIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return coretools.SymbolContext(ctx, r, repo, in)
	case "get_call_graph":
		var in coretools.CallGraphIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return coretools.CallGraph(ctx, r, repo, in)
	case "get_implementations":
		var in coretools.ImplementationsIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return coretools.Implementations(ctx, r, repo, in)
	case "file_outline":
		var in coretools.FileIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return coretools.FileOutline(ctx, r, repo, in)
	case "search":
		var in coretools.SearchIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return coretools.Search(ctx, r, repo, in)
	case "get_pr_context":
		var in coretools.PRContextIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		commit, base := w.base(ctx)
		if in.Diff == "" && len(in.Files) == 0 && in.Base == "" && commit != "" {
			if err := w.currentChange(ctx, commit, &in); err != nil {
				return nil, err
			}
		}
		return coretools.PRContextAgainst(ctx, r, base, short(commit), repo, in)
	case "change_checklist":
		var in coretools.ChecklistIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return coretools.ChangeChecklist(ctx, r, repo, in)
	case "verify_plan":
		var in coretools.VerifyPlanIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		if in.Diff == "" && len(in.Files) == 0 && in.Base == "" {
			if commit := w.baseCommitOf(ctx); commit != "" {
				change := coretools.PRContextIn{}
				if err := w.currentChange(ctx, commit, &change); err != nil {
					return nil, err
				}
				in.Diff, in.Files = change.Diff, change.Files
			}
		}
		return coretools.VerifyPlan(ctx, r, repo, in)
	}
	return nil, fmt.Errorf("%s is not answered from the working tree", name)
}
