// Package fixes turns accepted review comments into a fix branch: a model
// rewrites the affected files in a separate worktree, CodeSynapse commits
// the result, pushes it to a new codesynapse/* branch and opens a draft pull
// request, so CI runs and a person reviews the change before it is merged.
//
// Model output is untrusted. Only files named by the comments may change,
// CI and git configuration are never touched, nothing is force-pushed and
// the default branch is never written to.
package fixes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// Task is the LLM routing task for fixes.
const Task = "fix"

// maxFileBytes bounds the files a model is asked to rewrite.
const maxFileBytes = 256 << 10

// Store is what fixes needs from the review store.
type Store interface {
	GetReview(ctx context.Context, id string) (reviews.Review, error)
	SaveReview(ctx context.Context, r reviews.Review) error
}

// Runner makes fix branches.
type Runner struct {
	Authorize       func(context.Context) error
	CurrentRevision func(context.Context) (string, error)
	St              Store
	LLM             *llm.Registry
	Auth            gitx.Auth
	Author          gitx.Author
	// Workdir holds temporary worktrees.
	Workdir string
	// GitHubAPI is the GitHub REST base URL; empty means api.github.com.
	GitHubAPI string
	HTTP      *http.Client
	// Push replaces gitx.PushBranch in tests.
	Push func(ctx context.Context, wt, remote, branch string, auth gitx.Auth) error
}

// Request describes one fix run. Root, Remote, Base and BaseBranch come
// from the API, which has already checked that Root is a clone in the
// workspace and Remote is an allowed https remote.
type Request struct {
	ReviewID          string
	ValidationVersion int
	CommentIDs        []string
	Provider          string
	Root              string // the repository clone
	Remote            string // validated origin URL
	Base              string // commit to branch from (the indexed head)
	BaseBranch        string // branch the pull request targets
}

// Result is what a run produced.
type Result struct {
	Branch     string `json:"branch"`
	Commit     string `json:"commit"`
	PRURL      string `json:"pr_url,omitempty"`
	CompareURL string `json:"compare_url,omitempty"`
	PRError    string `json:"pr_error,omitempty"`
}

// protected paths are never changed by a generated fix: CI definitions run
// with repository secrets, and git metadata changes what checkouts do.
func protected(file string) bool {
	f := strings.ToLower(path.Clean(file))
	for _, p := range []string{".github/", ".gitlab-ci", ".circleci/", ".buildkite/", ".drone", ".woodpecker", "azure-pipelines", "jenkinsfile", ".git/", ".gitmodules", ".gitattributes", ".pre-commit-config"} {
		if strings.HasPrefix(f, p) || strings.Contains(f, "/"+p) {
			return true
		}
	}
	return false
}

// Selectable reports why c cannot be fixed, or "" when it can.
func Selectable(c reviews.CommentItem) string {
	switch {
	case c.Status != reviews.Accepted:
		return "only accepted comments are fixed"
	case c.Fix != nil && c.Fix.State == reviews.FixPushed:
		return "a fix is already pushed"
	case c.Fix != nil && (c.Fix.State == reviews.FixRunning || c.Fix.State == reviews.FixQueued) && !Stale(*c.Fix, time.Now()):
		return "a fix is already " + c.Fix.State
	case c.File == "" || strings.Contains(c.File, "..") || strings.HasPrefix(c.File, "/"):
		return "the comment has no repository file"
	case path.Ext(c.File) != ".go":
		return "automatic fixes currently support Go function bodies only"
	case protected(c.File):
		return "CI and git configuration files are never changed automatically"
	}
	return ""
}

// staleAfter is how long a queued or running fix may take before it is
// treated as abandoned (for example after a crash) and may be retried.
const staleAfter = 2 * time.Hour

// Stale reports whether an unfinished fix has been abandoned.
func Stale(f reviews.Fix, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, f.At)
	return err != nil || now.Sub(at) > staleAfter
}

// Run makes the fix branch. Comment fix state is saved as it goes, so the
// UI can show progress and failures.
func (r Runner) Run(ctx context.Context, req Request) (res Result, err error) {
	if _, err := execution.Require(ctx); err != nil {
		return res, err
	}
	if r.Authorize != nil {
		if err := r.Authorize(ctx); err != nil {
			return res, err
		}
	}
	rv, err := r.St.GetReview(ctx, req.ReviewID)
	if err != nil {
		return res, err
	}
	var targets []reviews.CommentItem
	for _, c := range rv.Comments {
		if slices.Contains(req.CommentIDs, c.ID) {
			check := c
			if check.Fix != nil && check.Fix.State == reviews.FixQueued {
				check.Fix = nil // queued by this run's request
			}
			if why := Selectable(check); why != "" {
				return res, fmt.Errorf("%s:%d: %s", c.File, c.Line, why)
			}
			targets = append(targets, c)
		}
	}
	if len(targets) == 0 || len(targets) != len(req.CommentIDs) {
		return res, errors.New("select distinct existing accepted comments to fix")
	}
	ids := make([]string, len(targets))
	for i, c := range targets {
		ids[i] = c.ID
	}
	log := logx.FromContext(ctx).With("review", req.ReviewID, "comments", len(ids))
	// From here on every failure is recorded on the comments.
	defer func() {
		if err != nil {
			log.Warn("fix failed", logx.Error(err))
			r.mark(context.WithoutCancel(ctx), req.ReviewID, ids, reviews.Fix{State: reviews.FixFailed, Error: err.Error()})
		}
	}()
	if req.ValidationVersion != ValidationVersion {
		return res, errors.New("fix validation policy changed; queue a new fix request")
	}
	if err := ValidateRevision(rv, req.Base); err != nil {
		return res, err
	}
	provider, err := r.LLM.ProviderFor(Task, req.Provider)
	if err != nil {
		return res, err
	}
	cfg, _ := r.LLM.Config(provider)
	if !cfg.Local() || !cfg.AllowsSource() {
		return res, fmt.Errorf("provider %s may not receive source code; use a local model for fixes", provider)
	}
	log = log.With("provider", provider)
	log.Info("fix started")
	r.mark(ctx, req.ReviewID, ids, reviews.Fix{State: reviews.FixRunning})

	wt := filepath.Join(r.Workdir, rv.ID+"-"+randHex(4))
	if err := os.MkdirAll(r.Workdir, 0o700); err != nil {
		return res, err
	}
	if err := gitx.AddWorktree(ctx, req.Root, wt, req.Base); err != nil {
		return res, err
	}
	defer func() { _ = gitx.RemoveWorktree(context.WithoutCancel(ctx), req.Root, wt) }() //nolint:errcheck // cleanup

	byFile := map[string][]reviews.CommentItem{}
	var files []string
	for _, c := range targets {
		f := path.Clean(c.File)
		if _, ok := byFile[f]; !ok {
			files = append(files, f)
		}
		byFile[f] = append(byFile[f], c)
	}
	root, err := os.OpenRoot(wt)
	if err != nil {
		return res, err
	}
	defer root.Close() //nolint:errcheck // read-only handle
	var model string
	var checked []checkedFile
	for _, f := range files {
		if err := r.recheck(ctx, req, rv, targets); err != nil {
			return res, err
		}
		log.Info("asking the model to fix a file", "file", f)
		m, output, err := r.fixFile(ctx, root, wt, req.Base, provider, cfg, f, byFile[f])
		if err != nil {
			return res, fmt.Errorf("%s: %w", f, err)
		}
		model = m
		checked = append(checked, output)
	}

	if err := r.recheck(ctx, req, rv, targets); err != nil {
		return res, err
	}
	if err := checkFiles(root, checked); err != nil {
		return res, err
	}
	msg := commitMessage(rv, targets, provider, model)
	commit, err := gitx.CommitFiles(ctx, wt, files, msg, r.Author)
	if err != nil {
		return res, err
	}
	if err := checkCommittedFiles(ctx, wt, req.Base, commit, checked); err != nil {
		return res, err
	}
	branch := fmt.Sprintf("%sfix-%s-%s", gitx.FixBranchPrefix, rv.ID[:min(8, len(rv.ID))], randHex(3))
	push := r.Push
	if push == nil {
		push = gitx.PushBranch
	}
	if err := r.recheck(ctx, req, rv, targets); err != nil {
		return res, err
	}
	if gitx.HeadCommit(ctx, wt) != commit {
		return res, errors.New("worktree commit changed before push")
	}
	log.Info("pushing fix branch", "branch", branch)
	if err := push(ctx, wt, req.Remote, branch, r.Auth); err != nil {
		return res, err
	}
	res = Result{Branch: branch, Commit: commit}
	if owner, repo, ok := r.github(req.Remote); ok {
		res.CompareURL = fmt.Sprintf("https://github.com/%s/%s/compare/%s...%s?expand=1", owner, repo, refPath(req.BaseBranch), refPath(branch))
		title, body, _ := strings.Cut(msg, "\n")
		pr, perr := r.openPR(ctx, owner, repo, title, strings.TrimSpace(body), branch, req.BaseBranch)
		if perr != nil {
			res.PRError = perr.Error()
		}
		res.PRURL = pr
	}
	r.mark(ctx, req.ReviewID, ids, reviews.Fix{State: reviews.FixPushed, Branch: branch, Commit: commit, PRURL: res.PRURL,
		CompareURL: res.CompareURL, Model: model, Error: res.PRError})
	return res, nil
}

func (r Runner) mark(ctx context.Context, id string, ids []string, f reviews.Fix) {
	if r.Authorize != nil && r.Authorize(ctx) != nil {
		return
	}
	rv, err := r.St.GetReview(ctx, id)
	if err != nil {
		return
	}
	reviews.SetFix(&rv, ids, f, time.Now())
	_ = r.St.SaveReview(ctx, rv) //nolint:errcheck // progress is best effort
}

const fixSystem = `You fix code review comments. You get one complete file and the comments to address in it.
Return the complete updated Go file. Change only the bodies of functions or methods identified by the accepted comments. Preserve signatures, imports, headers and all other bytes, including line endings. If a change needs another declaration, leave the file unchanged.
Never add network calls, credentials, telemetry or unrelated changes. If a comment cannot be fixed safely within this file, leave that code unchanged.
Answer with the full file inside one fenced code block and nothing else.`

func (r Runner) fixFile(ctx context.Context, root *os.Root, wt, base, provider string, cfg llm.ProviderConfig, file string, cs []reviews.CommentItem) (string, checkedFile, error) {
	var empty checkedFile
	orig, mode, err := readRegular(root, file)
	if err != nil {
		return "", empty, err
	}
	if err := checkOriginal(ctx, wt, base, file, orig); err != nil {
		return "", empty, err
	}
	// The whole file comes back, so it must fit the answer budget.
	if need := len(orig)/3 + 512; need > cfg.MaxOutputTokens {
		return "", empty, fmt.Errorf("file needs about %d output tokens; %s allows %d (raise max_output_tokens)", need, provider, cfg.MaxOutputTokens)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "File: %s\n\nComments to address:\n", file)
	for i, c := range cs {
		fmt.Fprintf(&b, "\n%d. line %d [%s, %s]: %s\n", i+1, c.Line, c.Severity, c.Category, c.Message)
		if c.Suggestion != "" {
			fmt.Fprintf(&b, "   Suggested fix: %s\n", c.Suggestion)
		}
	}
	fmt.Fprintf(&b, "\nCurrent file:\n````\n%s\n````\n", orig)
	resp, err := r.LLM.Complete(llm.WithTask(ctx, "fix"), provider, llm.Request{System: fixSystem, User: b.String()})
	if err != nil {
		return "", empty, err
	}
	out, err := extractFile(resp.Text)
	if err != nil {
		return "", empty, err
	}
	if !bytes.HasSuffix(orig, []byte("\n")) {
		out = strings.TrimSuffix(strings.TrimSuffix(out, "\n"), "\r")
	}
	if err := plausible(orig, []byte(out)); err != nil {
		return "", empty, err
	}
	if err := validateRewrite(file, orig, []byte(out), cs); err != nil {
		return "", empty, err
	}
	if r.Authorize != nil {
		if err := r.Authorize(ctx); err != nil {
			return "", empty, err
		}
	}
	before := checkedFile{name: file, hash: sha256.Sum256(orig), mode: mode}
	if err := checkFiles(root, []checkedFile{before}); err != nil {
		return "", empty, err
	}
	if err := root.WriteFile(file, []byte(out), mode.Perm()); err != nil {
		return "", empty, err
	}
	return resp.Model, checkedFile{name: file, hash: sha256.Sum256([]byte(out)), mode: mode}, nil
}

// extractFile accepts exactly one fenced file, preserving the enclosed bytes.
func extractFile(text string) (string, error) {
	if len(text) > maxFileBytes+256 {
		return "", errors.New("model response exceeds the automatic fix limit")
	}
	text = strings.TrimSpace(text)
	opening, rest, ok := strings.Cut(text, "\n")
	opening = strings.TrimSuffix(opening, "\r")
	fence := strings.TrimRight(opening, "go")
	if !ok || len(fence) < 3 || len(fence) > 8 || strings.Trim(fence, "`") != "" || (opening != fence && opening != fence+"go") {
		return "", errors.New("model must return one Go code block and no surrounding text")
	}
	offset := 0
	for _, line := range strings.SplitAfter(rest, "\n") {
		content := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if content == fence {
			if strings.TrimSpace(rest[offset+len(line):]) != "" {
				return "", errors.New("model returned more than one file or trailing text")
			}
			if offset > maxFileBytes {
				return "", errors.New("model file exceeds the automatic fix limit")
			}
			return rest[:offset], nil
		}
		offset += len(line)
	}
	return "", errors.New("model code block is not closed")
}

// plausible rejects answers that are clearly not an edited version of the
// file: empty, unchanged, or a different size altogether.
func plausible(orig, out []byte) error {
	switch {
	case len(out) > maxFileBytes:
		return errors.New("model file exceeds the automatic fix limit")
	case len(bytes.TrimSpace(out)) == 0:
		return errors.New("the model returned an empty file")
	case bytes.Equal(orig, out):
		return errors.New("the model made no change")
	case len(orig) > 2048 && (len(out) < len(orig)/2 || len(out) > len(orig)*3/2):
		return fmt.Errorf("the model's file is %d bytes against %d; refusing a rewrite this large", len(out), len(orig))
	}
	return nil
}

func commitMessage(rv reviews.Review, cs []reviews.CommentItem, provider, model string) string {
	var title string
	if len(cs) == 1 {
		title = "fix: " + firstSentence(cs[0].Message, 64)
	} else {
		title = fmt.Sprintf("fix: address %d accepted review comments", len(cs))
	}
	var b strings.Builder
	b.WriteString(title + "\n\n")
	fmt.Fprintf(&b, "From the CodeSynapse review %q (%s).\n\n", rv.Title, rv.ID)
	for _, c := range cs {
		fmt.Fprintf(&b, "- %s:%d [%s] %s\n", c.File, c.Line, c.Severity, firstSentence(c.Message, 160))
	}
	who := provider
	if model != "" {
		who += " (" + model + ")"
	}
	fmt.Fprintf(&b, "\nGenerated by %s. Fix validation policy %d. Check the change and CI before merging.\n", who, ValidationVersion)
	return b.String()
}

func firstSentence(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.IndexAny(s, ".!?"); i > 10 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit-1]) + "…"
	}
	return s
}

// github returns owner and repository when remote is on github.com and a
// token for it is configured.
func (r Runner) github(remote string) (owner, repo string, ok bool) {
	u, err := url.Parse(remote)
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") || !strings.EqualFold(r.Auth.Host, "github.com") || r.Auth.Token == "" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// openPR opens a draft pull request, falling back to a normal one where
// drafts are not available (private repositories on free plans).
func (r Runner) openPR(ctx context.Context, owner, repo, title, body, head, base string) (string, error) {
	api := r.GitHubAPI
	if api == "" {
		api = "https://api.github.com"
	}
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	var lastErr error
	for _, draft := range []bool{true, false} {
		payload, _ := json.Marshal(map[string]any{"title": title, "body": body, "head": head, "base": base, "draft": draft, "maintainer_can_modify": true}) //nolint:errchkjson // plain map
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/pulls", api, url.PathEscape(owner), url.PathEscape(repo)), bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+r.Auth.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		res, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("opening pull request: %w", err)
		}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20)) //nolint:errcheck // error body is informational
		_ = res.Body.Close()                                   //nolint:errcheck // read-only
		if res.StatusCode == http.StatusCreated {
			var out struct {
				HTMLURL string `json:"html_url"`
			}
			if err := json.Unmarshal(data, &out); err != nil {
				return "", err
			}
			return out.HTMLURL, nil
		}
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e) //nolint:errcheck // best effort
		lastErr = fmt.Errorf("GitHub refused the pull request (HTTP %d: %s); the branch is pushed, open it from the compare link", res.StatusCode, e.Message)
		if res.StatusCode != http.StatusUnprocessableEntity {
			break // retrying without draft only helps for "drafts not supported"
		}
	}
	return "", lastErr
}

// refPath escapes a branch name for a URL path, keeping its slashes.
func refPath(ref string) string { return strings.ReplaceAll(url.PathEscape(ref), "%2F", "/") }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}
