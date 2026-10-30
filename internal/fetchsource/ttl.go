package fetchsource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Limits on the per-host TTL file. The file is the developer's own, but it is
// parsed defensively anyway: a bad file is refused whole and the built-in
// list applies.
const (
	TTLFileName    = "fetch-source.json"
	MaxTTLFile     = 64 << 10
	MaxTTLHosts    = 200
	MinHostTTL     = time.Minute
	MaxHostTTL     = 30 * 24 * time.Hour
	maxHostPattern = 253
)

// TTLs are per-host cache lifetimes from the user's config folder, consulted
// before the built-in documentation list. The zero value and nil hold no
// rules, so TTL's built-in answer applies.
type TTLs struct {
	exact  map[string]time.Duration
	suffix map[string]time.Duration // "*.example.com" stored as ".example.com"
}

// ttlFile is the file's shape: {"hosts": {"docs.internal.example": "24h",
// "*.example.org": "168h"}}. Unknown fields are refused.
type ttlFile struct {
	Hosts map[string]string `json:"hosts"`
}

// DefaultTTLFile is the per-host TTL file in the user's config folder, the
// one the bridge keeps its API token in (~/Library/Application
// Support/codesynapse on macOS, $XDG_CONFIG_HOME/codesynapse on Linux).
func DefaultTTLFile() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no user config folder: %w", err)
	}
	return filepath.Join(base, "codesynapse", TTLFileName), nil
}

// LoadTTLs reads path. A missing file is no rules and no error; anything
// else that is wrong (not a regular file, too large, invalid) is an error,
// and the caller falls back to the built-in list.
func LoadTTLs(path string) (*TTLs, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the fetch_source TTL file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("the fetch_source TTL file is not a regular file")
	}
	f, err := os.Open(path) //nolint:gosec // the user's own config file
	if err != nil {
		return nil, fmt.Errorf("reading the fetch_source TTL file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read only
	b, err := io.ReadAll(io.LimitReader(f, MaxTTLFile+1))
	if err != nil {
		return nil, fmt.Errorf("reading the fetch_source TTL file: %w", err)
	}
	return ParseTTLs(b)
}

// ParseTTLs parses the TTL file. Host patterns are a lower-case host name
// ("docs.example.com") or "*." and a domain with at least two labels
// ("*.example.com", which matches its subdomains but not example.com
// itself); no ports, IP literals are not special. TTLs are Go durations
// between MinHostTTL and MaxHostTTL. At most MaxTTLHosts entries.
func ParseTTLs(b []byte) (*TTLs, error) {
	if len(b) > MaxTTLFile {
		return nil, fmt.Errorf("the fetch_source TTL file is larger than %d bytes", MaxTTLFile)
	}
	var in ttlFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		// The decoder's own messages can quote a host (a field path), so
		// only the kind of mistake is said.
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return nil, fmt.Errorf("the fetch_source TTL file is not valid JSON (at byte %d)", syntax.Offset)
		}
		return nil, errors.New(`the fetch_source TTL file is not valid: expected {"hosts": {"<host pattern>": "<duration>"}} and nothing else`)
	}
	if dec.More() {
		return nil, errors.New("the fetch_source TTL file has data after its object")
	}
	if len(in.Hosts) > MaxTTLHosts {
		return nil, fmt.Errorf("the fetch_source TTL file names more than %d hosts", MaxTTLHosts)
	}
	t := &TTLs{exact: map[string]time.Duration{}, suffix: map[string]time.Duration{}}
	for pattern, raw := range in.Hosts {
		d, err := time.ParseDuration(raw)
		if err != nil || d < MinHostTTL || d > MaxHostTTL {
			return nil, fmt.Errorf("the fetch_source TTL file has a TTL that is not a duration between %s and %s", MinHostTTL, MaxHostTTL)
		}
		if rest, ok := strings.CutPrefix(pattern, "*."); ok {
			if !validHost(rest) || !strings.Contains(rest, ".") {
				return nil, errInvalidPattern
			}
			t.suffix["."+rest] = d
			continue
		}
		if !validHost(pattern) {
			return nil, errInvalidPattern
		}
		t.exact[pattern] = d
	}
	return t, nil
}

// errInvalidPattern does not quote the pattern: errors end up in the bridge's
// log, which never names a host fetch_source is used for.
var errInvalidPattern = errors.New(`the fetch_source TTL file has a host pattern that is not a lower-case host name or "*." and a domain`)

// validHost accepts lower-case DNS names: labels of 1 to 63 letters, digits
// and hyphens, not starting or ending with a hyphen.
func validHost(h string) bool {
	if h == "" || len(h) > maxHostPattern {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range []byte(label) {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// For is the TTL for host: an exact entry, else the longest matching
// wildcard, else the built-in answer (TTL).
func (t *TTLs) For(host string) time.Duration {
	if t == nil {
		return TTL(host)
	}
	host = strings.ToLower(host)
	if d, ok := t.exact[host]; ok {
		return d
	}
	best, found := "", time.Duration(0)
	for s, d := range t.suffix {
		if strings.HasSuffix(host, s) && len(s) > len(best) {
			best, found = s, d
		}
	}
	if best != "" {
		return found
	}
	return TTL(host)
}

// Len is the number of host rules.
func (t *TTLs) Len() int {
	if t == nil {
		return 0
	}
	return len(t.exact) + len(t.suffix)
}
