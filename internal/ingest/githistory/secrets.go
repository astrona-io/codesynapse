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
// are skipped in both.
func ScanSecrets(ctx context.Context, repo, root string, limit int, g *model.Graph) error {
	out, err := gitx.LogPatches(ctx, root, limit, redact.CredentialPickaxe)
	if err != nil {
		return err
	}
	policy := repoconfig.FromContext(ctx)
	leaks, order := parsePatches(out, policy.PathAllowed)
	if len(leaks) == 0 {
		return nil
	}
	present, err := checkoutCredentials(ctx, root, policy)
	if err != nil {
		return err
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

// parsePatches reads LogPatches output (newest commit first) and returns,
// per credential, the oldest commit that added it, with the keys in the
// order first seen for a stable report.
func parsePatches(out []byte, allowed func(string) bool) (map[string]leak, []string) {
	leaks := map[string]leak{}
	var order []string
	for _, rec := range bytes.Split(out, []byte(gitx.PatchRecord)) {
		if len(rec) == 0 {
			continue
		}
		lines := strings.Split(string(rec), "\n")
		commit, at, _ := strings.Cut(lines[0], gitx.FieldSep)
		file, line, pem := "", 0, false
		add := func(c redact.Credential, n int) {
			k := credentialKey(c.Value)
			if _, seen := leaks[k]; !seen {
				order = append(order, k)
			}
			// Older commits come later and overwrite: the first addition wins.
			leaks[k] = leak{kind: c.Kind, commit: commit, time: at, file: file, line: n}
		}
		for _, l := range lines[1:] {
			switch {
			case strings.HasPrefix(l, "+++ "):
				file = patchPath(strings.TrimPrefix(l, "+++ "))
				if file != "" && allowed != nil && !allowed(file) {
					file = ""
				}
				pem = false
			case strings.HasPrefix(l, "@@ "):
				line = hunkStart(l)
				pem = false
			case strings.HasPrefix(l, "+") && file != "":
				text := l[1:]
				if pem {
					// The key's first line of base64 identifies it.
					if c, ok := redact.PrivateKeyBody(text); ok {
						add(c, line-1)
					}
				}
				for _, c := range redact.Credentials(text) {
					add(c, line)
				}
				pem = redact.PrivateKeyStart(text)
				line++
			}
		}
	}
	return leaks, order
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
func checkoutCredentials(ctx context.Context, root string, policy *repoconfig.Policy) (map[string]bool, error) {
	present := map[string]bool{}
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
		text := string(data)
		for _, c := range redact.Credentials(text) {
			present[credentialKey(c.Value)] = true
		}
		if redact.PrivateKeyHeader.MatchString(text) {
			lines := strings.Split(text, "\n")
			for i := 0; i+1 < len(lines); i++ {
				if redact.PrivateKeyStart(lines[i]) {
					if c, ok := redact.PrivateKeyBody(strings.TrimSuffix(lines[i+1], "\r")); ok {
						present[credentialKey(c.Value)] = true
					}
				}
			}
		}
		return nil
	})
	return present, err
}
