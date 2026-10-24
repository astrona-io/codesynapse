package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
)

// `codesynapse runtime upload` sends the coverage profiles and JUnit
// reports of a CI test run to the API (docs/PLAN.md, agent workflow roadmap
// step 11), which maps them onto the graph and keeps the counts for the
// organisation, pinned to the commit. The reports hold paths, line numbers,
// test names and counts, never source.

// runtimeUploadTimeout allows for large reports on a slow CI link.
const runtimeUploadTimeout = 2 * time.Minute

type runtimeOptions struct {
	EC       *app.Env
	upload   bool
	repo     string
	apiURL   string
	commit   string
	coverage []string
	junit    []string
	root     string
}

func (o *runtimeOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.repo, "repo", "", "Repository name in CodeSynapse (default: matched from the checkout's origin remote)")
	f.StringVar(&o.apiURL, "api-url", app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"), "CodeSynapse API")
	if o.upload {
		f.StringVar(&o.commit, "commit", "", "Commit the reports measured (default: the checkout's HEAD)")
		f.StringSliceVar(&o.coverage, "coverage", nil, "Go coverage profile or LCOV file (default: cover.out, lcov.info and the like at the root)")
		f.StringSliceVar(&o.junit, "junit", nil, "JUnit XML report (default: junit.xml, report.xml and the like at the root)")
	}
	return nil
}

func (o *runtimeOptions) Complete(ctx context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	if err != nil {
		return err
	}
	o.root = abs
	if o.upload && o.commit == "" {
		o.commit = gitx.HeadCommit(ctx, o.root)
	}
	return nil
}

func (o *runtimeOptions) Validate(context.Context, *app.Env) error {
	if o.upload && o.commit == "" {
		return errors.New("no commit: run inside the checkout the tests ran in, or pass --commit")
	}
	return nil
}

func (o *runtimeOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	c := newAPIClient(o.apiURL)
	c.http.Timeout = runtimeUploadTimeout
	repo := o.repo
	if repo == "" {
		var err error
		if repo, err = c.resolveRepo(ctx, o.root); err != nil {
			return err
		}
	}
	path := "/api/v1/repos/" + url.PathEscape(repo) + "/runtime"
	var sum runtimeSummary
	if !o.upload {
		if err := c.do(ctx, http.MethodGet, path, nil, &sum); err != nil {
			return err
		}
		return o.print(repo, sum)
	}
	body, err := o.body()
	if err != nil {
		return err
	}
	if err := c.send(ctx, http.MethodPut, path, body, &sum); err != nil {
		return err
	}
	return o.print(repo, sum)
}

// body reads the reports into the gzipped upload.
func (o *runtimeOptions) body() ([]byte, error) {
	files := runtimefacts.Files{Coverage: o.coverage, JUnit: o.junit}
	if files.Empty() {
		files = runtimefacts.Discover(o.root)
	}
	if files.Empty() {
		return nil, errors.New("no reports found: pass --coverage or --junit, or run the tests with -coverprofile=cover.out first")
	}
	type report struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	var reports []report
	add := func(kind string, paths []string) error {
		for _, p := range paths {
			st, err := os.Stat(p)
			if err != nil {
				return err
			}
			if st.Size() > runtimefacts.MaxReportBytes {
				return fmt.Errorf("%s: %w", p, runtimefacts.ErrTooLarge)
			}
			b, err := os.ReadFile(p) //nolint:gosec // a report the user named or left at a known place
			if err != nil {
				return err
			}
			reports = append(reports, report{Kind: kind, Name: filepath.Base(p), Content: string(b)})
		}
		return nil
	}
	if err := add("coverage", files.Coverage); err != nil {
		return nil, err
	}
	if err := add("junit", files.JUnit); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(map[string]any{"commit": o.commit, "reports": reports})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runtimeSummary is the API's answer (api.RuntimeSummary).
type runtimeSummary struct {
	Commit         string    `json:"commit"`
	MeasuredAt     time.Time `json:"measured_at"`
	Sources        []string  `json:"sources"`
	Unit           string    `json:"unit"`
	Functions      int       `json:"functions"`
	Uncovered      int       `json:"uncovered"`
	Tests          int       `json:"tests"`
	Failed         int       `json:"failed"`
	Flaky          []string  `json:"flaky"`
	UnmatchedFiles int       `json:"unmatched_files"`
	UnmatchedTests int       `json:"unmatched_tests"`
	Stale          bool      `json:"stale"`
}

func (o *runtimeOptions) print(repo string, s runtimeSummary) error {
	if o.EC.Output == app.OutputJSON {
		return json.NewEncoder(o.EC.Stdout).Encode(s)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: test run at %s (%s)\n", repo, s.Commit, strings.Join(s.Sources, ", "))
	fmt.Fprintf(&b, "  %d functions measured, %d not executed by any test\n", s.Functions, s.Uncovered)
	fmt.Fprintf(&b, "  %d tests, %d failed, %d flaky\n", s.Tests, s.Failed, len(s.Flaky))
	for _, f := range s.Flaky {
		fmt.Fprintf(&b, "    flaky: %s\n", f)
	}
	if s.UnmatchedFiles+s.UnmatchedTests > 0 {
		fmt.Fprintf(&b, "  not in the graph: %d files, %d tests (vendored, generated or another module)\n", s.UnmatchedFiles, s.UnmatchedTests)
	}
	if s.Stale {
		b.WriteString("  the graph indexed another commit: answers say so until the next sync\n")
	}
	_, err := io.WriteString(o.EC.Stdout, b.String())
	return err
}

func runtimeCmd(ec *app.Env) *cobra.Command {
	parent := &cobra.Command{Use: "runtime", Short: "Upload measured coverage and test results from CI", GroupID: app.GroupBase}
	parent.AddCommand(app.NewSubCommand("upload [path]", &runtimeOptions{EC: ec, upload: true}, ec).
		WithShortDesc("Send a test run's coverage profiles and JUnit reports to CodeSynapse").
		WithLongDesc("Reads Go coverage profiles, LCOV tracefiles and JUnit XML (named with --coverage and --junit, or found at the usual places in the checkout) and sends them, gzipped, to the API with the key from CODESYNAPSE_API_TOKEN. The API maps them onto the graph and keeps the counts for your organisation, pinned to the commit; get_symbol_context, get_pr_context and verify_plan then show measured coverage and flaky tests.").
		WithExample("  go test ./... -coverprofile=cover.out\n  codesynapse runtime upload --repo acme/api\n  codesynapse runtime upload --coverage coverage/lcov.info --junit reports/junit.xml --commit $GITHUB_SHA").
		WithMaxArgs(1).Build())
	parent.AddCommand(app.NewSubCommand("show [path]", &runtimeOptions{EC: ec}, ec).
		WithShortDesc("Show the test run CodeSynapse has for a repository").
		WithMaxArgs(1).Build())
	return parent
}
