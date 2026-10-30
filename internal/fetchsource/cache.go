package fetchsource

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultCacheBytes caps the cache folder; the least recently used entries
// go first.
const DefaultCacheBytes = 200 << 20

// maxEntryFile bounds the read of one cache file: the text of a 2 MiB page
// plus its fields, with room for JSON escaping.
const maxEntryFile = 3*MaxBody + 64<<10

// Entry is one cached page. The text has already passed redact.Free.
type Entry struct {
	URL          string        `json:"url"`
	ETag         string        `json:"etag,omitempty"`
	LastModified string        `json:"last_modified,omitempty"`
	Fetched      time.Time     `json:"fetched"`
	TTL          time.Duration `json:"ttl"`
	ContentType  string        `json:"content_type"`
	RawBytes     int           `json:"raw_bytes"`
	Text         string        `json:"text"`
}

// Cache keeps fetched pages as files in one folder that only the user can
// read (0700, files 0600), one per SHA-256 of the normalised URL. A nil
// Cache stores nothing.
type Cache struct {
	dir string
	max int64
	mu  sync.Mutex
}

// DefaultDir is the cache folder in the user's cache directory
// (~/Library/Caches/codesynapse/sources on macOS,
// $XDG_CACHE_HOME/codesynapse/sources on Linux).
func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no user cache folder: %w", err)
	}
	return filepath.Join(base, "codesynapse", "sources"), nil
}

// OpenCache uses dir, creating it, holding at most maxBytes of entries
// (DefaultCacheBytes when not positive).
func OpenCache(dir string, maxBytes int64) (*Cache, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the source cache: %w", err)
	}
	// An existing folder may have been created with a looser mode.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs x to be entered; 0700 is owner only
		return nil, fmt.Errorf("securing the source cache: %w", err)
	}
	return &Cache{dir: dir, max: maxBytes}, nil
}

func (c *Cache) path(key string) string { return filepath.Join(c.dir, key+".json") }

// Get returns the entry for key if it holds url.
func (c *Cache) Get(key, url string) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	p := c.path(key)
	// Only a regular file of ours: never follow a link planted in the folder.
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return Entry{}, false
	}
	f, err := os.Open(p) //nolint:gosec // a hex key inside the cache folder
	if err != nil {
		return Entry{}, false
	}
	defer f.Close() //nolint:errcheck // read only
	raw, err := io.ReadAll(io.LimitReader(f, maxEntryFile+1))
	if err != nil || len(raw) > maxEntryFile {
		return Entry{}, false
	}
	var e Entry
	if json.Unmarshal(raw, &e) != nil || e.URL != url {
		return Entry{}, false
	}
	return e, true
}

// Touch marks key as just used, for eviction.
func (c *Cache) Touch(key string) {
	if c == nil {
		return
	}
	now := time.Now()
	_ = os.Chtimes(c.path(key), now, now) //nolint:errcheck // only eviction order depends on it
}

// Put stores e under key, replacing the file atomically, and evicts the
// least recently used entries over the cap.
func (c *Cache) Put(key string, e Entry) error {
	if c == nil {
		return nil
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := os.CreateTemp(c.dir, ".tmp-*") // 0600
	if err != nil {
		return fmt.Errorf("writing the source cache: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(raw)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, c.path(key))
	}
	if werr != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best effort
		return fmt.Errorf("writing the source cache: %w", werr)
	}
	c.evict()
	return nil
}

// evict removes entries, least recently used first, until the folder is
// under its cap, and temporary files a crashed write left behind.
func (c *Cache) evict() {
	des, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type file struct {
		path string
		size int64
		used time.Time
	}
	var files []file
	var total int64
	for _, de := range des {
		fi, err := de.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		p := filepath.Join(c.dir, de.Name())
		if strings.HasPrefix(de.Name(), ".tmp-") {
			if time.Since(fi.ModTime()) > time.Hour {
				_ = os.Remove(p) //nolint:errcheck // best effort
			}
			continue
		}
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		files = append(files, file{p, fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	if total <= c.max {
		return
	}
	slices.SortFunc(files, func(a, b file) int { return cmp.Or(a.used.Compare(b.used), cmp.Compare(a.path, b.path)) })
	for _, f := range files {
		if total <= c.max {
			return
		}
		if err := os.Remove(f.path); err == nil || errors.Is(err, os.ErrNotExist) {
			total -= f.size
		}
	}
}
