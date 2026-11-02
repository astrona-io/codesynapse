package githistory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// The history secret scan reads the patches of up to CommitLimit commits on
// every ingest, which takes seconds on a repository with a long history even
// when nothing changed. The cache keeps, per repository, the commits the last
// scan read with their hits and the credential keys of the checkout's file
// contents. It holds hashes, paths, line numbers and credential kinds only,
// never a credential value or file content, the same as the findings in the
// graph. Anything wrong with it (another scanner, an unreadable or edited
// file) makes the scan read everything again.

// secretCacheVersion changes when what parseCommits or fileCredentials
// return for the same input changes without model.IndexVersion changing.
const secretCacheVersion = 1

const (
	maxSecretCacheBytes = 64 << 20
	maxCachedHits       = 1000
	maxCachedKind       = 100
)

type secretCache struct {
	Key     string              `json:"key"`
	Head    string              `json:"head"`
	Commits []scannedCommit     `json:"commits"`
	Files   map[string][]string `json:"files,omitempty"`
}

// secretCacheKey names what produced a cache, so a scan never reuses one
// made with another pickaxe, limit or analysis.
func secretCacheKey(limit int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d\x00%d\x00%d\x00%s", secretCacheVersion, model.IndexVersion, limit, redact.CredentialPickaxe))
	return hex.EncodeToString(sum[:])
}

// contentDigest identifies a file's content in the cache.
func contentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// loadSecretCache returns the cache at path for limit, or nil when there is
// none or it cannot be trusted.
func loadSecretCache(path string, limit int) *secretCache {
	if path == "" {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // the caller's cache file, outside the checkout
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSecretCacheBytes+1))
	if err != nil || len(data) > maxSecretCacheBytes {
		return nil
	}
	var c secretCache
	if json.Unmarshal(data, &c) != nil || c.Key != secretCacheKey(limit) || !c.valid(limit) {
		return nil
	}
	return &c
}

// valid checks what a scan would otherwise take from git as it is.
func (c *secretCache) valid(limit int) bool {
	if !isHex(c.Head, 40, 64) || len(c.Commits) > limit || len(c.Files) > maxCheckoutFiles {
		return false
	}
	for _, cm := range c.Commits {
		if !isHex(cm.Hash, 40, 64) || len(cm.Time) > 40 || len(cm.Hits) > maxCachedHits {
			return false
		}
		for _, h := range cm.Hits {
			if !isHex(h.Key, 16, 16) || len(h.Kind) > maxCachedKind || !filepath.IsLocal(filepath.FromSlash(h.File)) {
				return false
			}
		}
	}
	for digest, keys := range c.Files {
		if !isHex(digest, 32, 32) || len(keys) > maxCachedHits {
			return false
		}
		for _, k := range keys {
			if !isHex(k, 16, 16) {
				return false
			}
		}
	}
	return true
}

// save writes the cache for limit to path atomically, readable by this
// user only.
func (c *secretCache) save(path string, limit int) error {
	c.Key = secretCacheKey(limit)
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if len(data) > maxSecretCacheBytes {
		return fmt.Errorf("cache of %d bytes is over the limit", len(data))
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".secrets-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// isHex reports whether s is lowercase hex of length min or max.
func isHex(s string, minLen, maxLen int) bool {
	if len(s) != minLen && len(s) != maxLen {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
