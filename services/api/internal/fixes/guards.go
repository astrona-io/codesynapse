package fixes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/reviews"
)

// ValidationVersion pins queued requests to the checks implemented by this runner.
const ValidationVersion = 1

// ValidateRevision refuses reviews without an unambiguous immutable source revision.
// Base is the diff's old side and does not identify the reviewed file contents.
func ValidateRevision(rv reviews.Review, base string) error {
	if !commitHash(base) || rv.Commit == "" && rv.Head == "" {
		return errors.New("review has no pinned commit; create a review of the current indexed revision")
	}
	for _, rev := range []string{rv.Commit, rv.Head} {
		if rev != "" && (!commitHash(rev) || rev != base) {
			return errors.New("review revision differs from the fix base; review the current indexed revision")
		}
	}
	return nil
}

func commitHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

func (r Runner) recheck(ctx context.Context, req Request, before reviews.Review, targets []reviews.CommentItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.Authorize != nil {
		if err := r.Authorize(ctx); err != nil {
			return err
		}
	}
	if gitx.HeadCommit(ctx, req.Root) != req.Base {
		return errors.New("repository head changed; sync and review the current revision")
	}
	if r.CurrentRevision != nil {
		head, err := r.CurrentRevision(ctx)
		if err != nil {
			return err
		}
		if head != req.Base {
			return errors.New("indexed revision changed while the fix was running")
		}
	}
	now, err := r.St.GetReview(ctx, req.ReviewID)
	if err != nil {
		return err
	}
	if now.ID != before.ID || now.Repo != before.Repo || now.OrgID != before.OrgID || now.Commit != before.Commit || now.Head != before.Head || now.Base != before.Base {
		return errors.New("review changed while the fix was running")
	}
	if err := ValidateRevision(now, req.Base); err != nil {
		return err
	}
	for _, expected := range targets {
		expected.Fix = nil
		matches := 0
		for _, c := range now.Comments {
			if c.ID == expected.ID {
				c.Fix = nil
				if c != expected || c.Status != reviews.Accepted {
					return errors.New("accepted comment changed while the fix was running")
				}
				matches++
			}
		}
		if matches != 1 {
			return errors.New("accepted comment is no longer available")
		}
	}
	return nil
}

type checkedFile struct {
	name string
	hash [sha256.Size]byte
	mode fs.FileMode
}

// readRegular refuses symlink aliases even when they remain inside the worktree.
func readRegular(root *os.Root, name string) ([]byte, fs.FileMode, error) {
	parts := strings.Split(name, "/")
	for i := range parts {
		st, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, 0, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, 0, errors.New("symlink files and directories cannot be fixed automatically")
		}
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxFileBytes {
		return nil, 0, errors.New("automatic fixes require a regular file no larger than 256 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > maxFileBytes {
		return nil, 0, errors.New("file grew beyond the automatic fix limit")
	}
	return data, st.Mode(), nil
}

func checkFiles(root *os.Root, files []checkedFile) error {
	for _, f := range files {
		data, mode, err := readRegular(root, f.name)
		if err != nil {
			return err
		}
		if sha256.Sum256(data) != f.hash || mode != f.mode {
			return fmt.Errorf("%s changed after validation", f.name)
		}
	}
	return nil
}

func checkCommittedFiles(ctx context.Context, wt, base, commit string, files []checkedFile) error {
	parents, err := gitx.Run(ctx, wt, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return err
	}
	if fields := strings.Fields(string(parents)); len(fields) != 2 || fields[0] != commit || fields[1] != base {
		return errors.New("fix commit does not have the reviewed revision as its sole parent")
	}
	changed, err := gitx.Run(ctx, wt, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", base, commit, "--")
	if err != nil {
		return err
	}
	paths := strings.Split(strings.TrimSuffix(string(changed), "\x00"), "\x00")
	if len(paths) != len(files) {
		return errors.New("fix commit changes unexpected files")
	}
	allowed := make(map[string]bool, len(files))
	for _, f := range files {
		allowed[f.name] = true
	}
	for _, name := range paths {
		if !allowed[name] {
			return errors.New("fix commit changes unexpected files")
		}
	}
	for _, f := range files {
		entry, err := gitx.Run(ctx, wt, "ls-tree", "-z", commit, "--", f.name)
		if err != nil {
			return err
		}
		meta, name, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
		mode := "100644"
		if f.mode.Perm()&0111 != 0 {
			mode = "100755"
		}
		fields := strings.Fields(meta)
		if !ok || name != f.name || len(fields) != 3 || fields[0] != mode || fields[1] != "blob" {
			return errors.New("committed file mode differs from the validated output")
		}
		data, err := gitx.Run(ctx, wt, "show", commit+":"+f.name)
		if err != nil {
			return err
		}
		if sha256.Sum256(data) != f.hash {
			return errors.New("committed file differs from the validated output")
		}
	}
	return nil
}

func checkOriginal(ctx context.Context, wt, base, name string, data []byte) error {
	original, err := gitx.Run(ctx, wt, "show", base+":"+name)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, original) {
		return errors.New("checkout file differs from the reviewed commit")
	}
	return nil
}
