package vuln

import (
	"io/fs"
	"os"
	"path/filepath"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
)

type manifestScope struct {
	root   string
	policy *repoconfig.Policy
}

// Auxiliary manifests use the same policy as the lockfile that led to them.
func readManifest(path string, scopes ...manifestScope) ([]byte, error) {
	if len(scopes) > 0 {
		rel, err := filepath.Rel(scopes[0].root, path)
		if err != nil || !scopes[0].policy.PathAllowed(filepath.ToSlash(rel)) {
			return nil, fs.ErrPermission
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxManifest {
		return nil, fs.ErrInvalid
	}
	return os.ReadFile(path) //nolint:gosec // bounded regular manifest within the discovered root
}
