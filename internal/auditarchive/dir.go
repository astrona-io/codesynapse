package auditarchive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Dir keeps segment files in a local directory (a volume in compose). It
// has no object lock: files are made read-only, which stops mistakes, not
// someone with access to the host.
type Dir struct{ root string }

// NewDir creates the directory if needed.
func NewDir(root string) (*Dir, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("audit archive directory: %w", err)
	}
	return &Dir{root: abs}, nil
}

// Backend implements Archive.
func (d *Dir) Backend() string { return "dir" }

// Put implements Archive: written to a temporary file, synced, then renamed,
// so a reader never sees half a segment.
func (d *Dir) Put(_ context.Context, key string, body []byte, _ time.Time) error {
	if err := checkKey(key); err != nil {
		return err
	}
	f, err := os.CreateTemp(d.root, ".seal-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint:errcheck // gone after the rename
	if _, err := f.Write(body); err != nil {
		f.Close() //nolint:errcheck,gosec // the write error wins
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck,gosec // the sync error wins
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o400); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(d.root, key))
}

// Get implements Archive.
func (d *Dir) Get(_ context.Context, key string) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(d.root, key)) //nolint:gosec // checkKey allows only the segment key format, so it stays in root
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err == nil && len(b) > MaxFileBytes {
		err = errors.New("audit archive file is too large")
	}
	return b, err
}

// Delete implements Archive.
func (d *Dir) Delete(_ context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(d.root, key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
