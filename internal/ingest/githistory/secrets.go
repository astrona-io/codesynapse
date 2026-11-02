package githistory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/rules"
)

// Secrets in git history (security roadmap item 6): a credential that was
// committed and later removed from the code is still in every clone, so it
// needs rotating, not only deleting. git log -G picks the commits whose
// patches match a credential shape (redact.CredentialPickaxe, patterns
// only); their added lines are checked with redact.Credentials, and a
// credential still in the checkout is left to the rules that read the
// checkout. Values never leave memory: findings name the kind, the commit
// and the place, and a credential is told apart only by a truncated hash.

// RuleIDs are the rules this detector reports; see rules/config/<id>.yaml.
var RuleIDs = []string{historyRule}

const (
	historyRule         = "git-history-secret"
	maxHistoryFindings  = 50
	maxCheckoutFileSize = 2 << 20
	maxCheckoutFiles    = 50_000
)

// leak is where a credential was first added.
type leak struct {
	kind, commit, time, file string
	line                     int
}

// ScanSecrets reports credentials added in the last limit commits that are
// no longer in the checkout at root. Paths the repository policy excludes
// are skipped in both. cacheFile, when not empty, keeps what this scan found
// for the next one (see secretcache.go): commits never change, so a later
// scan reads only the commits that are new since, and checkout files whose
// content it has seen before are not scanned again. It must not be inside
// root, and it holds no credential values.
func ScanSecrets(ctx context.Context, repo, root string, limit int, cacheFile string, g *model.Graph) error {
	cache := loadSecretCache(cacheFile, limit)
	commits, head, err := scanCommits(ctx, root, limit, cache)
	if err != nil {
		return err
	}
	policy := repoconfig.FromContext(ctx)
	leaks, order := assemble(commits, policy.PathAllowed)
	next := &secretCache{Head: head, Commits: commits}
	if cache != nil {
		next.Files = cache.Files
	}
	var present map[string]bool
	if len(leaks) > 0 {
		present, next.Files, err = checkoutCredentials(ctx, root, policy, next.Files)
		if err != nil {
			return err
		}
	}
	if cacheFile != "" && head != "" {
		if err := next.save(cacheFile, limit); err != nil {
			logx.FromContext(ctx).Warn("history secret scan cache not saved", logx.Error(err))
		}
	}
	if len(leaks) == 0 {
		return nil
	}
	r := rules.MustGet(historyRule)
	reported := 0
	for _, h := range order {
		l := leaks[h]
		if present[h] || reported >= maxHistoryFindings {
			continue
		}
		reported++
		owner := model.NodeID(repo, model.KindCommit, l.commit)
		if _, ok := g.Node(owner); !ok {
			owner = model.NodeID(repo, model.KindRepository, repo)
		}
		date, _, _ := strings.Cut(l.time, "T")
		id := model.NodeID(repo, model.KindFinding, "history:"+h)
		g.AddNode(model.Node{
			ID: id, Kind: model.KindFinding, Repo: repo, Name: r.Title, Rule: r.ID, Severity: r.Severity, CWE: r.CWE,
			File: l.file, StartLine: l.line, EndLine: l.line, Language: "git-history",
			Summary: redact.Summary(fmt.Sprintf("%s added in commit %s (%s) and no longer in the checkout. %s", l.kind, l.commit[:min(12, len(l.commit))], date, r.Fix), 300),
		})
		g.AddEdge(model.Edge{From: owner, To: id, Kind: model.EdgeHasFinding})
	}
	return nil
}

// credentialKey identifies a credential without keeping it.
func credentialKey(value string) string {
	sum := sha256.Sum256([]byte("codesynapse/history-secret\x00" + value))
	return hex.EncodeToString(sum[:8])
}

// hit is a credential on an added line of a commit. File is the path before
// the repository policy is applied, so a cached hit stays right when the
// policy changes.
type hit struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// scannedCommit is a commit the pickaxe matched and its hits, often none.
// It counts towards the limit either way, as it does in git log -n.
type scannedCommit struct {
	Hash string `json:"hash"`
	Time string `json:"time"`
	Hits []hit  `json:"hits,omitempty"`
}

// parsePatches reads LogPatches output (newest commit first) and returns,
// per credential, the oldest commit that added it, with the keys in the
// order first seen for a stable report.
func parsePatches(out []byte, allowed func(string) bool) (map[string]leak, []string) {
	return assemble(parseCommits(out), allowed)
}

// parseCommits reads LogPatches output into its commits, in the same order.
func parseCommits(out []byte) []scannedCommit {
	var commits []scannedCommit
	for _, rec := range bytes.Split(out, []byte(gitx.PatchRecord)) {
		if len(rec) == 0 {
			continue
		}
		lines := strings.Split(string(rec), "\n")
		hash, at, _ := strings.Cut(lines[0], gitx.FieldSep)
		c := scannedCommit{Hash: hash, Time: at}
		file, line, pem := "", 0, false
		add := func(cred redact.Credential, n int) {
			c.Hits = append(c.Hits, hit{Kind: cred.Kind, Key: credentialKey(cred.Value), File: file, Line: n})
		}
		for _, l := range lines[1:] {
			switch {
			case strings.HasPrefix(l, "+++ "):
				file = patchPath(strings.TrimPrefix(l, "+++ "))
				pem = false
			case strings.HasPrefix(l, "@@ "):
				line = hunkStart(l)
				pem = false
			case strings.HasPrefix(l, "+") && file != "":
				text := l[1:]
				if pem {
					// The key's first line of base64 identifies it.
					if cred, ok := redact.PrivateKeyBody(text); ok {
						add(cred, line-1)
					}
				}
				for _, cred := range redact.Credentials(text) {
					add(cred, line)
				}
				pem = redact.PrivateKeyStart(text)
				line++
			}
		}
		commits = append(commits, c)
	}
	return commits
}

// assemble keeps, per credential in a file allowed admits, the oldest commit
// that added it; commits are newest first.
func assemble(commits []scannedCommit, allowed func(string) bool) (map[string]leak, []string) {
	leaks := map[string]leak{}
	var order []string
	for _, c := range commits {
		for _, h := range c.Hits {
			if allowed != nil && !allowed(h.File) {
				continue
			}
			if _, seen := leaks[h.Key]; !seen {
				order = append(order, h.Key)
			}
			// Older commits come later and overwrite: the first addition wins.
			leaks[h.Key] = leak{kind: h.Kind, commit: c.Hash, time: c.Time, file: h.File, line: h.Line}
		}
	}
	return leaks, order
}

// scanCommits returns the last limit commits the pickaxe matches, newest
// first, and the HEAD they were read from ("" when it is unknown, which
// keeps them out of the cache). With a cache of an ancestor of HEAD only the
// commits since are read; anything else (a force push, another branch, a
// shallow clone that lost the cached commit) reads them all.
func scanCommits(ctx context.Context, root string, limit int, cache *secretCache) ([]scannedCommit, string, error) {
	head := gitx.HeadCommit(ctx, root)
	if head == "" {
		out, err := gitx.LogPatches(ctx, root, limit, redact.CredentialPickaxe)
		return parseCommits(out), "", err
	}
	from := ""
	if cache != nil {
		if cache.Head == head {
			return cache.Commits, head, nil
		}
		if ok, err := gitx.IsAncestor(ctx, root, cache.Head, head); err == nil && ok {
			from = cache.Head
		}
	}
	out, err := gitx.LogPatchesRange(ctx, root, limit, redact.CredentialPickaxe, from, head)
	if err != nil {
		return nil, "", err
	}
	commits := parseCommits(out)
	if from != "" {
		commits = append(commits, cache.Commits...)
		commits = commits[:min(len(commits), limit)]
	}
	return commits, head, nil
}

// patchPath is the new path of a "+++ b/path" line, "" for a deletion.
func patchPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(p, `"`) {
		if u, err := strconv.Unquote(p); err == nil {
			p = u
		}
	}
	return strings.TrimPrefix(p, "b/")
}

// hunkStart is the new-file line a "@@ -a,b +c,d @@" hunk starts at.
func hunkStart(h string) int {
	_, rest, ok := strings.Cut(h, " +")
	if !ok {
		return 0
	}
	n, _, _ := strings.Cut(rest, " ")
	n, _, _ = strings.Cut(n, ",")
	v, _ := strconv.Atoi(n)
	return v
}

// checkoutCredentials are the keys of the credentials in the checkout's
// text files: those are reported, if at all, by the checkout's own rules.
// seen maps a content digest to the keys found in it by an earlier scan;
// the returned map is the same for the files of this checkout.
func checkoutCredentials(ctx context.Context, root string, policy *repoconfig.Policy, seen map[string][]string) (map[string]bool, map[string][]string, error) {
	present := map[string]bool{}
	next := map[string][]string{}
	files := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry cannot hold the credential
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if allowed, policyErr := policy.WalkEntry(root, p, d); !allowed || policyErr != nil {
			return policyErr
		}
		if d.IsDir() {
			if p != root && d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if files++; files > maxCheckoutFiles {
			return filepath.SkipAll
		}
		if info, err := d.Info(); err != nil || info.Size() > maxCheckoutFileSize {
			return nil //nolint:nilerr // see above
		}
		data, err := os.ReadFile(p) //nolint:gosec // a regular file found by the walk under root
		if err != nil || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			return nil //nolint:nilerr // unreadable or binary
		}
		digest := contentDigest(data)
		keys, ok := seen[digest]
		if !ok {
			keys = fileCredentials(string(data))
		}
		next[digest] = keys
		for _, k := range keys {
			present[k] = true
		}
		return nil
	})
	return present, next, err
}

// fileCredentials are the keys of the credentials in one file's text.
func fileCredentials(text string) []string {
	var keys []string
	for _, c := range redact.Credentials(text) {
		keys = append(keys, credentialKey(c.Value))
	}
	if redact.PrivateKeyHeader.MatchString(text) {
		lines := strings.Split(text, "\n")
		for i := 0; i+1 < len(lines); i++ {
			if redact.PrivateKeyStart(lines[i]) {
				if c, ok := redact.PrivateKeyBody(strings.TrimSuffix(lines[i+1], "\r")); ok {
					keys = append(keys, credentialKey(c.Value))
				}
			}
		}
	}
	return keys
}
