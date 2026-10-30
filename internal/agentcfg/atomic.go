package agentcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// WritePrivate replaces p atomically with a 0600 file: a reader sees the old
// file or the new one, never half of one.
func WritePrivate(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-"+filepath.Base(p)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Settings lock: hooks of several Claude Code sessions can change
// settings.json at the same moment, and each read-change-write must see the
// one before it.
const (
	lockWait  = 5 * time.Second
	lockStale = 30 * time.Second
	lockPoll  = 20 * time.Millisecond
)

// withFileLock runs fn while holding file+".codesynapse.lock", created with
// O_EXCL. A lock older than lockStale was left by a process that died and is
// taken over.
func withFileLock(file string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	lock := file + ".codesynapse.lock"
	deadline := time.Now().Add(lockWait)
	for {
		fh, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // beside the user's own settings file
		if err == nil {
			_ = fh.Close()
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("locking %s: %w", file, err)
		}
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > lockStale {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is being changed by another process (lock %s)", file, lock)
		}
		time.Sleep(lockPoll)
	}
	defer os.Remove(lock) //nolint:errcheck // a leftover goes stale
	return fn()
}
