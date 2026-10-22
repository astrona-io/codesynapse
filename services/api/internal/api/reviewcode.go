package api

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/parisnakitakejser/codesynapse/internal/forge"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/redact"
)

// Limits for the code shown next to review comments. The code is read from
// the checkout when asked for and never stored.
const (
	codeContext     = 3  // lines shown around the commented range
	codeMaxSpan     = 80 // lines per comment, context included
	codeMaxComments = 200
	codeMaxFile     = 1 << 20
	codeMaxLineLen  = 400
)

type codeLine struct {
	N    int    `json:"n"`
	Text string `json:"text"`
}

// commentCode is the code around one comment. Ref is the commit it was read
// at, or "" when it came from the working tree (which may have moved on).
type commentCode struct {
	Lines []codeLine `json:"lines,omitempty"`
	Ref   string     `json:"ref,omitempty"`
	URL   string     `json:"url,omitempty"`
	Error string     `json:"error,omitempty"`
}

// handleReviewCode returns, per comment id, the redacted lines around the
// comment and a link to them on the repository's forge.
func (s *Server) handleReviewCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rv, err := s.st.GetReview(ctx, r.PathValue("id"))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	n, err := s.st.Node(ctx, model.NodeID(rv.Repo, model.KindRepository, rv.Repo))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	root := ""
	if n.Root != "" && (s.inWorkspace(n.Root) || s.insideSources(n.Root) == nil) {
		root = n.Root
	}
	ref := rv.Commit
	if gitx.ValidateRev(ref) != nil {
		ref = ""
	}
	link := s.forgeLinker(n.URL, cmp.Or(ref, n.Head, n.Branch))
	files := map[string]fileLines{}
	out := map[string]commentCode{}
	for i, c := range rv.Comments {
		if i == codeMaxComments {
			break
		}
		if c.Line <= 0 || !cleanRelPath(c.File) {
			continue
		}
		end := max(c.EndLine, c.Line)
		cc := commentCode{}
		if link != nil {
			cc.URL = link(c.File, c.Line, end)
		}
		if root == "" {
			cc.Error = "the repository's checkout is not available to the API"
		} else {
			fl, ok := files[c.File]
			if !ok {
				fl = readFileLines(ctx, root, ref, c.File)
				files[c.File] = fl
			}
			if fl.err != nil {
				cc.Error = fl.err.Error()
			} else {
				cc.Ref = fl.ref
				cc.Lines = window(fl.lines, c.Line, end)
			}
		}
		out[c.ID] = cc
	}
	writeJSON(w, 200, map[string]any{"code": out})
}

type fileLines struct {
	lines []string
	ref   string
	err   error
}

// readFileLines reads file at ref, falling back to the working tree when the
// commit is unknown or missing (shallow clones drop old commits).
func readFileLines(ctx context.Context, root, ref, file string) fileLines {
	if ref != "" {
		if b, err := blobAt(ctx, root, ref, file); err == nil {
			return fileLines{lines: splitLines(b), ref: ref}
		}
	}
	b, err := readInside(root, file)
	if err != nil {
		return fileLines{err: err}
	}
	return fileLines{lines: splitLines(b)}
}

func blobAt(ctx context.Context, root, ref, file string) ([]byte, error) {
	obj := ref + ":" + file
	size, err := gitx.Run(ctx, root, "cat-file", "-s", obj)
	if err != nil {
		return nil, err
	}
	if n, err := strconv.Atoi(strings.TrimSpace(string(size))); err != nil || n > codeMaxFile {
		return nil, errors.New("file is too large to show")
	}
	return gitx.Run(ctx, root, "cat-file", "blob", obj)
}

// readInside reads root/file without following a symlink out of root.
func readInside(root, file string) ([]byte, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errors.New("checkout not found")
	}
	p, err := filepath.EvalSymlinks(filepath.Join(realRoot, filepath.FromSlash(file)))
	if err != nil {
		return nil, errors.New("file not found in the checkout")
	}
	if rel, err := filepath.Rel(realRoot, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("file is outside the repository")
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("file not found in the checkout")
	}
	if fi.Size() > codeMaxFile {
		return nil, errors.New("file is too large to show")
	}
	return os.ReadFile(p) //nolint:gosec // resolved and checked to be inside root
}

func splitLines(b []byte) []string {
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return nil // binary
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// window returns the lines around [line, end], redacted and truncated.
func window(lines []string, line, end int) []codeLine {
	if line > len(lines) {
		return nil
	}
	from := max(1, line-codeContext)
	to := min(len(lines), end+codeContext, from+codeMaxSpan-1)
	out := make([]codeLine, 0, to-from+1)
	for n := from; n <= to; n++ {
		t := redact.Text(lines[n-1])
		if utf8.RuneCountInString(t) > codeMaxLineLen {
			t = string([]rune(t)[:codeMaxLineLen]) + "…"
		}
		out = append(out, codeLine{N: n, Text: strings.TrimSuffix(t, "\r")})
	}
	return out
}

// cleanRelPath accepts repo-relative slash paths without "..".
func cleanRelPath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && !strings.Contains(p, "\\") && path.Clean(p) == p &&
		!slices.Contains(strings.Split(p, "/"), "..")
}

// forgeLinker returns a function building links to file lines at ref on the
// repository's forge, or nil when the remote or provider is unknown.
func (s *Server) forgeLinker(remote, ref string) func(file string, line, end int) string {
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || ref == "" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	kind, ok := forge.KindForHost(host)
	if !ok {
		for _, o := range s.orgs.list() {
			if o.Host == host {
				kind, ok = o.Provider, true
				break
			}
		}
	}
	repoPath := strings.Trim(strings.TrimSuffix(u.EscapedPath(), ".git"), "/")
	if !ok || repoPath == "" {
		return nil
	}
	base := "https://" + u.Host + "/" + repoPath
	isCommit := gitx.ValidateRev(ref) == nil && len(ref) >= 7 && strings.Trim(ref, "0123456789abcdef") == ""
	refEsc := escapeSegments(ref)
	return func(file string, line, end int) string {
		f := escapeSegments(file)
		switch kind {
		case forge.GitLab:
			if end > line {
				return fmt.Sprintf("%s/-/blob/%s/%s#L%d-%d", base, refEsc, f, line, end)
			}
			return fmt.Sprintf("%s/-/blob/%s/%s#L%d", base, refEsc, f, line)
		case forge.Forgejo:
			src := "src/branch/"
			if isCommit {
				src = "src/commit/"
			}
			return base + "/" + src + refEsc + "/" + f + lineAnchor(line, end)
		default:
			return base + "/blob/" + refEsc + "/" + f + lineAnchor(line, end)
		}
	}
}

func lineAnchor(line, end int) string {
	if end > line {
		return fmt.Sprintf("#L%d-L%d", line, end)
	}
	return fmt.Sprintf("#L%d", line)
}

func escapeSegments(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
