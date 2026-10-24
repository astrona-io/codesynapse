package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/taskeval"
)

// evalOptions configures `codesynapse eval`.
type evalOptions struct {
	EC            *app.Env
	root          string
	base, head    string
	maxFiles      int
	baseline      string
	tolerance     float64
	writeBaseline bool
	results       bool
	repo          string
	allowNetwork  bool
	sessions      bool
	cases         string
}

// errRegression fails the run (exit 1) after the report and the list of
// regressions are printed.
var errRegression = errors.New("the task eval fell below its baseline")

func (o *evalOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.base, "base", "", "Commit the graph is built at; tasks are the commits after it (default: the baseline's)")
	f.StringVar(&o.head, "head", "", "Last commit to turn into a task (default: the baseline's, else HEAD)")
	f.IntVar(&o.maxFiles, "max-files", 25, "Skip commits that touch more files than this")
	f.StringVar(&o.baseline, "baseline", "", "JSON baseline to compare with (and to take --base and --head from)")
	f.Float64Var(&o.tolerance, "tolerance", 0.02, "How far recall may fall, or tokens grow as a share, before it is a regression")
	f.BoolVar(&o.writeBaseline, "write-baseline", false, "Write this run's summary to --baseline instead of comparing")
	f.BoolVar(&o.results, "results", false, "Include every task's answer files in JSON output")
	f.StringVar(&o.repo, "repo", "", "Repository name in the report (default: the directory name)")
	f.BoolVar(&o.allowNetwork, "allow-network", false, "Let go list download the modules base needs (CI; otherwise only the module cache is used)")
	f.BoolVar(&o.sessions, "sessions", false, "Score the cases captured from your agent sessions ("+evalCaptureEnv+"=true) instead of commits")
	f.StringVar(&o.cases, "cases", "", "Cases file for --sessions (default: "+casesFileName+" in the checkout's git directory)")
	return nil
}

func (o *evalOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	o.root = abs
	return err
}

func (o *evalOptions) Validate(context.Context, *app.Env) error {
	if o.sessions {
		if o.baseline != "" || o.base != "" || o.head != "" || o.writeBaseline {
			return errors.New("--sessions scores captured cases; leave out --base, --head and --baseline")
		}
		return nil
	}
	if o.cases != "" {
		return errors.New("--cases needs --sessions")
	}
	if o.writeBaseline && o.baseline == "" {
		return errors.New("--write-baseline needs --baseline")
	}
	if o.baseline == "" && o.base == "" {
		return errors.New("--base or --baseline is required")
	}
	if o.tolerance < 0 || o.tolerance > 1 {
		return errors.New("--tolerance must be between 0 and 1")
	}
	return nil
}

func (o *evalOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	if o.sessions {
		return o.runSessions(ctx)
	}
	var baseline taskeval.Report
	if o.baseline != "" && !o.writeBaseline {
		raw, err := os.ReadFile(o.baseline)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &baseline); err != nil {
			return fmt.Errorf("reading %s: %w", o.baseline, err)
		}
		o.base, o.head = cmpOr(o.base, baseline.Base), cmpOr(o.head, baseline.Head)
	}
	base, err := commitOf(ctx, o.root, o.base)
	if err != nil {
		return err
	}
	head, err := commitOf(ctx, o.root, cmpOr(o.head, "HEAD"))
	if err != nil {
		return err
	}

	// The graph is built at base, in a worktree of its own, so no answer
	// can draw on the change it is asked about.
	repo := cmpOr(o.repo, filepath.Base(o.root))
	g, err := ingest.AtCommit(ctx, o.root, base, ingest.Options{Repo: repo, AllowNetwork: o.allowNetwork})
	if err != nil {
		return err
	}
	mem := memory.New()
	if err := mem.ReplaceRepo(ctx, repo, g); err != nil {
		return err
	}

	commits, err := taskeval.Commits(ctx, o.root, base, head)
	if err != nil {
		return err
	}
	tasks := taskeval.Tasks(commits, taskeval.IndexedFiles(g), taskeval.Options{MaxFiles: o.maxFiles})
	if len(tasks) == 0 {
		return fmt.Errorf("no tasks in %s..%s", short(base), short(head))
	}
	rep := taskeval.Run(ctx, mem, repo, tasks)
	rep.Base, rep.Head = base, head

	if o.writeBaseline {
		summary := rep
		summary.Results = nil
		raw, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(o.baseline, append(raw, '\n'), 0o644); err != nil { //nolint:gosec // a committed report
			return err
		}
	}
	if !o.results {
		rep.Results = nil
	}
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		writeEval(o.EC, rep, commits)
	}
	if o.baseline == "" || o.writeBaseline {
		return nil
	}
	regressions := taskeval.Regressions(rep, baseline, o.tolerance)
	if rep.Tasks != baseline.Tasks {
		o.EC.Logger.Warn("task count differs from the baseline; compare with care", "tasks", rep.Tasks, "baseline", baseline.Tasks)
	}
	for _, r := range regressions {
		fmt.Fprintln(o.EC.Stderr, "regression:", r) //nolint:errcheck // terminal
	}
	if len(regressions) > 0 {
		return errRegression
	}
	return nil
}

func writeEval(ec *app.Env, rep taskeval.Report, commits []taskeval.Commit) {
	w := ec.Stdout
	fmt.Fprintf(w, "Task eval of %s: %d tasks from %d commits after %s (graph built there), up to %s\n\n", rep.Repo, rep.Tasks, len(commits), short(rep.Base), short(rep.Head)) //nolint:errcheck // terminal
	fmt.Fprintf(w, "%-16s %9s %10s %7s %7s %7s\n", "tool", "recall@5", "recall@10", "hit@10", "tokens", "errors")                                                               //nolint:errcheck // terminal
	for _, tool := range []string{taskeval.ToolMinimalContext, taskeval.ToolSearch} {
		s := rep.Summary[tool]
		fmt.Fprintf(w, "%-16s %9.3f %10.3f %7.3f %7.0f %7d\n", tool, s.Recall5, s.Recall10, s.Hit10, s.Tokens, s.Errors) //nolint:errcheck // terminal
	}
}

// runSessions scores the cases captured from agent sessions: the graph is
// built at each commit a session began at, and the cases of that commit
// are asked against it.
func (o *evalOptions) runSessions(ctx context.Context) error {
	path := o.cases
	if path == "" {
		if path = casesFile(ctx, o.root); path == "" {
			return errors.New("not a git checkout: name the cases file with --cases")
		}
	}
	cases, err := taskeval.ReadCases(path)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no cases in %s: start Claude Code with %s=true and the CodeSynapse plugin, then work as usual", path, evalCaptureEnv)
	}
	byBase := map[string][]taskeval.Task{}
	var bases []string
	for _, c := range cases {
		if _, ok := byBase[c.Base]; !ok {
			bases = append(bases, c.Base)
		}
		byBase[c.Base] = append(byBase[c.Base], c.AsTask())
	}
	repo := cmpOr(o.repo, filepath.Base(o.root))
	var parts []taskeval.Report
	for _, base := range bases {
		commit, err := commitOf(ctx, o.root, base)
		if err != nil {
			o.EC.Logger.Warn("skipping cases of a commit this checkout lacks", "commit", short(base), "cases", len(byBase[base]))
			continue
		}
		g, err := ingest.AtCommit(ctx, o.root, commit, ingest.Options{Repo: repo, AllowNetwork: o.allowNetwork})
		if err != nil {
			return err
		}
		mem := memory.New()
		if err := mem.ReplaceRepo(ctx, repo, g); err != nil {
			return err
		}
		parts = append(parts, taskeval.Run(ctx, mem, repo, byBase[base]))
	}
	rep := taskeval.Merge(repo, parts)
	if rep.Tasks == 0 {
		return errors.New("no case could be scored: their commits are not in this checkout")
	}
	if !o.results {
		rep.Results = nil
	}
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	w := o.EC.Stdout
	fmt.Fprintf(w, "Session eval of %s: %d cases from %s, graphs built at the %d commits they began at\n\n", rep.Repo, rep.Tasks, path, len(parts)) //nolint:errcheck // terminal
	fmt.Fprintf(w, "%-16s %9s %10s %7s %7s %7s\n", "tool", "recall@5", "recall@10", "hit@10", "tokens", "errors")                                   //nolint:errcheck // terminal
	for _, tool := range []string{taskeval.ToolMinimalContext, taskeval.ToolSearch} {
		s := rep.Summary[tool]
		fmt.Fprintf(w, "%-16s %9.3f %10.3f %7.3f %7.0f %7d\n", tool, s.Recall5, s.Recall10, s.Hit10, s.Tokens, s.Errors) //nolint:errcheck // terminal
	}
	return nil
}

// commitOf resolves rev to a full commit ID.
func commitOf(ctx context.Context, dir, rev string) (string, error) {
	if err := gitx.ValidateRev(rev); err != nil {
		return "", err
	}
	out, err := gitx.Run(ctx, dir, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func short(id string) string { return id[:min(12, len(id))] }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func evalCmd(ec *app.Env) *cobra.Command {
	o := &evalOptions{EC: ec}
	return app.NewSubCommand("eval [path]", o, ec).
		WithShortDesc("Score minimal_context and search against the files past commits touched (offline)").
		WithLongDesc("Builds the graph at --base and turns each later commit into a task: its subject is the question, the files it changed that existed at base are the answer. Reports recall@5, recall@10, hit@10 and returned tokens per tool. With --baseline it fails when a score falls below the committed one; docs/PLAN.md, agent workflow roadmap step 1. With --sessions it scores the cases captured from your own agent sessions instead (start Claude Code with " + evalCaptureEnv + "=true; the plugin keeps each session's first prompt and the files it changed in the checkout's git directory, never sending them).").
		WithExample("  codesynapse eval . --base HEAD~100\n  codesynapse eval . --baseline eval/codesynapse.json\n  codesynapse eval . --base <sha> --baseline eval/codesynapse.json --write-baseline\n  codesynapse eval . --sessions --results").
		WithGroupID(app.GroupBase).
		WithMaxArgs(1).
		Build()
}
