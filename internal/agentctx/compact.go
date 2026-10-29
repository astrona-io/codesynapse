package agentctx

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// CompactID drops repo from a node ID of that repository and writes its
// root Go module as "~" ("acme/app|Function|example.com/app/x.F" becomes
// "Function|~/x.F"). IDs of other repositories and strings that are not
// node IDs are returned unchanged.
func CompactID(id, repo, module string) string {
	rest, ok := strings.CutPrefix(id, repo+"|")
	if !ok || repo == "" {
		return id
	}
	kind, local, ok := strings.Cut(rest, "|")
	if !ok || local == "" || !model.NodeKind(kind).Valid() {
		return id
	}
	return kind + "|" + ShortenModule(local, module)
}

// CompactAnswer rewrites a JSON answer about repo with compact IDs:
// CompactID for every string, ShortenModule for "package" paths. The
// answer does not state the module: measured on this repository, that
// cost more than it saved, since most answers write "~" once or not at
// all. The server instructions say what "~" is, once per session.
func CompactAnswer(raw []byte, repo, module string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep integers exact through the round trip
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return json.Marshal(compactValue(doc, "", repo, module))
}

func compactValue(v any, key, repo, module string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = compactValue(e, k, repo, module)
		}
	case []any:
		for i, e := range x {
			x[i] = compactValue(e, key, repo, module)
		}
	case string:
		out := CompactID(x, repo, module)
		if key == "package" && out == x {
			out = ShortenModule(x, module)
		}
		return out
	}
	return v
}

// Around a module path in a node ID: "(*" and "(" open a receiver or a
// route's handler package, "[" and "," type arguments; "/" and "."
// continue a package path or name a member.
const (
	moduleBefore = "(*[, "
	moduleAfter  = "/.)],"
)

// atBoundary reports whether s[start:end] stands alone as a package path.
func atBoundary(s string, start, end int) bool {
	return (start == 0 || strings.IndexByte(moduleBefore, s[start-1]) >= 0) &&
		(end == len(s) || strings.IndexByte(moduleAfter, s[end]) >= 0)
}

// hasTilde reports whether local has a "~" that ExpandModule replaces.
func hasTilde(local string) bool {
	for i := range len(local) {
		if local[i] == '~' && atBoundary(local, i, i+1) {
			return true
		}
	}
	return false
}

// ShortenModule writes module, the repository's root Go module path, as
// "~" wherever it stands as a package path in a node ID's local part:
// "github.com/acme/app/x.F" is "~/x.F", a method
// "(*github.com/acme/app/x.T).M" is "(*~/x.T).M", the root package
// "github.com/acme/app" is "~", a route "GET /x (github.com/acme/app/api)"
// is "GET /x (~/api)". A longer module that only starts the same
// ("github.com/acme/app-tools") is left alone, and so is a local that
// already has such a "~", which could not be told apart when expanding.
// Other languages' IDs never contain a Go module path, so they stay as
// they are.
func ShortenModule(local, module string) string {
	if module == "" || !strings.Contains(local, module) || hasTilde(local) {
		return local
	}
	var b strings.Builder
	for i := 0; i < len(local); {
		if strings.HasPrefix(local[i:], module) && atBoundary(local, i, i+len(module)) {
			b.WriteByte('~')
			i += len(module)
			continue
		}
		b.WriteByte(local[i])
		i++
	}
	return b.String()
}

// ExpandModule undoes ShortenModule.
func ExpandModule(local, module string) string {
	if module == "" || !hasTilde(local) {
		return local
	}
	var b strings.Builder
	for i := range len(local) {
		if local[i] == '~' && atBoundary(local, i, i+1) {
			b.WriteString(module)
			continue
		}
		b.WriteByte(local[i])
	}
	return b.String()
}
