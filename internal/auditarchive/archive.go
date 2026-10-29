// Package auditarchive seals the oldest audit entries into archive files,
// in a directory or in S3-compatible object storage, and checks them later.
// PostgreSQL keeps a hot window of entries plus every segment's hashes, so
// the chain verifies across segments without reading the files.
package auditarchive

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Archive stores sealed segment files. Keys are segmentKey names.
type Archive interface {
	Backend() string // dir or s3
	// Put stores body under key; retainUntil is when the file may be
	// deleted (object lock where the backend has it).
	Put(ctx context.Context, key string, body []byte, retainUntil time.Time) error
	Get(ctx context.Context, key string) ([]byte, error) // at most MaxFileBytes
	Delete(ctx context.Context, key string) error        // a missing file is not an error
}

// Bounds of a segment and of what is read back.
const (
	SegmentEntries = 20_000
	MaxFileBytes   = 256 << 20
	maxPlainBytes  = 1 << 30
	maxLineBytes   = 1 << 20
)

// Prompt log files (promptlog.go) carry a prefix; row IDs are unique across
// organisations, so the names never collide.
var keyPattern = regexp.MustCompile(`^(promptlog-)?[0-9]{20}-[0-9]{20}\.jsonl\.gz$`)

func segmentKey(first, last int64) string { return fmt.Sprintf("%020d-%020d.jsonl.gz", first, last) }

func checkKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("audit archive: invalid key %q", key)
	}
	return nil
}

// encode writes entries (audit events or prompt log rows) as gzip JSON
// lines, oldest first.
func encode[T any](events []T) ([]byte, error) {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	enc := json.NewEncoder(zw)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// decode reads a segment file back, bounded in lines, line length and size.
func decode(body []byte) ([]accounts.Event, error) {
	return decodeLines[accounts.Event](body, SegmentEntries, maxLineBytes)
}

func decodeLines[T any](body []byte, most, maxLine int) ([]T, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(io.LimitReader(zr, maxPlainBytes))
	sc.Buffer(make([]byte, 64<<10), maxLine)
	var out []T
	for sc.Scan() {
		if len(out) == most {
			return nil, errors.New("segment file holds more entries than a segment can")
		}
		var e T
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
