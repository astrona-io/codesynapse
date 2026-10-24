package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
)

const maxRuntimeReceipts = 4096

type runtimeReceipt struct {
	ID      string    `json:"id"`
	Primary string    `json:"primary"`
	Expires time.Time `json:"expires"`
}

func runtimeReceiptDir(c apiClient, client string) (string, error) {
	root := os.Getenv("CODESYNAPSE_RUNTIME_DIR")
	if root == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(cache, "codesynapse", "runtime")
	}
	if !filepath.IsAbs(root) {
		return "", execution.ErrInvalid
	}
	// Separate installations and keys cannot consume each other's receipts.
	hash := sha256.Sum256([]byte(c.base + "\x00" + c.token))
	return filepath.Join(root, client, hex.EncodeToString(hash[:16])), nil
}

func receiptName(kind, id string) string {
	hash := sha256.Sum256([]byte(id))
	return kind + "-" + hex.EncodeToString(hash[:]) + ".json"
}

func writeRuntimeReceipt(c apiClient, client, kind, id, primary string) error {
	if c.token == "" || (client != "codex" && client != "cursor") || (kind != "call" && kind != "thread") || len(id) == 0 || len(id) > 256 || execution.Validate(execution.Identity{TenantID: "t", SubjectID: "u", SessionID: primary, ParentSessionID: primary}) != nil {
		return execution.ErrInvalid
	}
	dir, err := runtimeReceiptDir(c, client)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Bound disk use, and retire abandoned call receipts without retaining
	// prompts, arguments or results. Thread lineage shares session expiry.
	d, err := os.Open(dir) //nolint:gosec // private cache directory derived from fixed client and credential hash
	if err != nil {
		return err
	}
	entries, readErr := d.ReadDir(maxRuntimeReceipts + 1)
	_ = d.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	count := 0
	now := time.Now()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil {
			return statErr
		}
		ttl := 30 * time.Minute
		if strings.HasPrefix(e.Name(), "thread-") {
			ttl = 7 * 24 * time.Hour
		}
		if now.Sub(info.ModTime()) > ttl {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		count++
	}
	if count >= maxRuntimeReceipts {
		return errors.New("runtime receipt limit reached")
	}
	expires := now.Add(30 * time.Minute)
	if kind == "thread" {
		expires = now.Add(7 * 24 * time.Hour)
	}
	data, err := json.Marshal(runtimeReceipt{ID: id, Primary: primary, Expires: expires})
	if err != nil {
		return err
	}
	if len(data) > 1024 {
		return execution.ErrInvalid
	}
	file, err := os.CreateTemp(dir, ".receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name()) //nolint:errcheck // temporary metadata only
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	target := filepath.Join(dir, receiptName(kind, id))
	// Link publishes a complete immutable receipt, and never overwrites a
	// concurrent mapping of the same runtime call to another chat.
	if err = os.Link(file.Name(), target); err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	existing, err := readRuntimeReceipt(c, client, kind, id, false)
	if err != nil {
		return err
	}
	if existing != primary {
		return execution.ErrConflict
	}
	return nil
}

func readRuntimeReceipt(c apiClient, client, kind, id string, consume bool) (string, error) {
	if id == "" || len(id) > 256 {
		return "", execution.ErrMissing
	}
	dir, err := runtimeReceiptDir(c, client)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, receiptName(kind, id))
	info, err := os.Lstat(path) //nolint:gosec // fixed receipt kind and hashed ID inside private credential-scoped cache
	if err != nil {
		return "", errors.Join(execution.ErrMissing, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 1024 {
		return "", execution.ErrInvalid
	}
	data, err := os.ReadFile(path) //nolint:gosec // hashed name in the private user cache
	if err != nil {
		return "", execution.ErrMissing
	}
	var receipt runtimeReceipt
	if json.Unmarshal(data, &receipt) != nil || receipt.ID != id || time.Now().After(receipt.Expires) {
		return "", execution.ErrInvalid
	}
	if execution.Validate(execution.Identity{TenantID: "t", SubjectID: "u", SessionID: receipt.Primary, ParentSessionID: receipt.Primary}) != nil {
		return "", execution.ErrInvalid
	}
	if consume {
		// Removal is the claim: at most one concurrent call consumes a receipt.
		if os.Remove(path) != nil { //nolint:gosec // claim the same validated private receipt
			return "", execution.ErrMissing
		}
	}
	return receipt.Primary, nil
}
