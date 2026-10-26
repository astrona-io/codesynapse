package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/gittools"
	"github.com/astrona-io/codesynapse/internal/gitx"
)

// ask_local (internal/asklocal) is answered here: the bridge reads the files
// from the checkout, which the server cannot see, and the API has a local
// model answer. Only files git tracks are read, so ignored files such as
// .env never leave the checkout. The call is reported like the git tools.

// askLocalTimeout bounds one answer. Local models are slow on large inputs,
// and the time is spent to save the agent's tokens.
const askLocalTimeout = 30 * time.Minute

// answerAskLocal answers a tools/call for ask_local and reports whether it did.
func (b *bridge) answerAskLocal(ctx context.Context, msg []byte, m rpcMessage) bool {
	var c toolCall
	if json.Unmarshal(msg, &c) != nil || c.Params.Name != asklocal.Tool || len(m.ID) == 0 {
		return false
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, askLocalTimeout)
	defer cancel()

	result, failed := b.askLocal(ctx, msg, c)
	out, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	if err != nil {
		out = errorResponse(m.ID, err)
		failed = true
	}
	b.write(out)
	b.reportLocal(ctx, asklocal.Tool, time.Since(start), len(out)/4, failed)
	return true
}

func (b *bridge) askLocal(ctx context.Context, msg []byte, c toolCall) (map[string]any, bool) {
	fail := func(err error) (map[string]any, bool) {
		msg, _, _ := strings.Cut(err.Error(), "\n")
		return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": msg}}}, true
	}
	var in asklocal.In
	dec := json.NewDecoder(bytes.NewReader(c.Params.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fail(fmt.Errorf("%w: %w", asklocal.ErrInvalid, err))
	}
	dir, err := b.checkout(ctx)
	if err != nil {
		return fail(err)
	}
	files, skipped, err := readTracked(ctx, dir, in.Paths)
	if err != nil {
		return fail(err)
	}
	req := asklocal.Request{Question: in.Question, Files: files}
	if err := req.Validate(); err != nil {
		return fail(err)
	}
	repo := b.checkoutRepo(ctx)
	if repo == "" {
		return fail(errors.New("this checkout is not indexed in CodeSynapse; read the files yourself"))
	}
	session := b.session
	if b.routes != nil {
		chat, err := b.routes.forCall(ctx, msg)
		if err != nil {
			return fail(err)
		}
		session = chat.session
	}
	ectx, err := b.report.executionContext(ctx, session, session)
	if err != nil {
		return fail(err)
	}
	api := b.report
	api.http = &http.Client{} // the context bounds it; the default client gives up after seconds
	var ans asklocal.Answer
	if err := api.do(ectx, http.MethodPost, "/api/v1/repos/"+url.PathEscape(repo)+"/ask-local", req, &ans); err != nil {
		return fail(fmt.Errorf("ask_local: %w", err))
	}
	if len(skipped) > 0 {
		ans.Note = strings.TrimSpace(ans.Note + " Not read (binary, too large or over the file limit): " + strings.Join(skipped, ", ") + ".")
	}
	raw, err := json.Marshal(ans)
	if err != nil {
		return fail(err)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(raw)}},
		"structuredContent": json.RawMessage(raw),
		"_meta":             map[string]any{"codesynapse/source": "local-model"},
	}, false
}

// readTracked reads the files paths name in the checkout at dir: files,
// directories (their tracked files) and file:start-end ranges. Untracked
// and ignored files are refused, symlinks and binaries skipped.
func readTracked(ctx context.Context, dir string, paths []string) ([]asklocal.File, []string, error) {
	if len(paths) == 0 {
		return nil, nil, fmt.Errorf("%w: paths is empty", asklocal.ErrInvalid)
	}
	if len(paths) > asklocal.MaxPaths {
		return nil, nil, fmt.Errorf("%w: more than %d paths", asklocal.ErrInvalid, asklocal.MaxPaths)
	}
	var files []asklocal.File
	var skipped []string
	total := 0
	seen := map[string]bool{}
	for _, spec := range paths {
		p, start, end, err := asklocal.ParseSpec(spec)
		if err != nil {
			return nil, nil, err
		}
		if p, err = gittools.CleanPath(p); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", asklocal.ErrInvalid, err)
		}
		out, err := gitx.Run(ctx, dir, "ls-files", "-z", "--", p)
		if err != nil {
			return nil, nil, err
		}
		tracked := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
		if len(tracked) == 1 && tracked[0] == "" {
			return nil, nil, fmt.Errorf("%w: %s is not a file or directory git tracks", asklocal.ErrInvalid, p)
		}
		if start > 0 && (len(tracked) != 1 || tracked[0] != p) {
			return nil, nil, fmt.Errorf("%w: a line range needs a single file, not %s", asklocal.ErrInvalid, p)
		}
		slices.Sort(tracked)
		for _, f := range tracked {
			if seen[f+fmt.Sprint(start, end)] {
				continue
			}
			seen[f+fmt.Sprint(start, end)] = true
			if len(files) >= asklocal.MaxFiles {
				skipped = append(skipped, f)
				continue
			}
			file, ok, err := readFile(dir, f, start, end)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				skipped = append(skipped, f)
				continue
			}
			if total += len(file.Text); total > asklocal.MaxTotalBytes {
				return nil, nil, fmt.Errorf("%w: more than %d bytes; ask about fewer files or line ranges", asklocal.ErrInvalid, asklocal.MaxTotalBytes)
			}
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		return nil, skipped, fmt.Errorf("%w: none of the paths is a readable text file", asklocal.ErrInvalid)
	}
	return files, skipped, nil
}

// readFile reads lines start..end (all for 0) of a tracked file. ok is false
// for symlinks, binaries and files over the size limit.
func readFile(dir, rel string, start, end int) (asklocal.File, bool, error) {
	full := filepath.Join(dir, filepath.FromSlash(rel))
	fi, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) {
		return asklocal.File{}, false, nil // deleted in the working tree
	}
	if err != nil {
		return asklocal.File{}, false, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > asklocal.MaxFileBytes {
		return asklocal.File{}, false, nil
	}
	data, err := os.ReadFile(full) //nolint:gosec // a tracked file of the checkout, path cleaned and listed by git
	if err != nil {
		return asklocal.File{}, false, err
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return asklocal.File{}, false, nil
	}
	lines := strings.SplitAfter(strings.ToValidUTF8(string(data), "�"), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return asklocal.File{}, false, nil
	}
	if start == 0 {
		start, end = 1, len(lines)
	}
	if start > len(lines) {
		return asklocal.File{}, false, fmt.Errorf("%w: %s has %d lines, the range starts at %d", asklocal.ErrInvalid, rel, len(lines), start)
	}
	end = min(end, len(lines))
	return asklocal.File{Path: rel, Start: start, End: end, Text: strings.Join(lines[start-1:end], "")}, true, nil
}
