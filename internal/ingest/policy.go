package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
)

// ErrPolicyApproval prevents a repository from approving its own coverage loss.
var ErrPolicyApproval = errors.New("repository exclusions require approval of the current configuration digest")

// PolicyMetadata records requests and actual coverage without applying tenant suppressions.
type PolicyMetadata struct {
	Present                 bool                    `json:"present"`
	Digest                  string                  `json:"digest,omitempty"`
	Config                  repoconfig.RepoConfig   `json:"config"`
	Diagnostics             []repoconfig.Diagnostic `json:"diagnostics,omitempty"`
	ExclusionsApproved      bool                    `json:"exclusions_approved"`
	ExcludedFiles           int                     `json:"excluded_files"`
	KubernetesExcludedFiles int                     `json:"kubernetes_excluded_files"`
	KubernetesDisabled      bool                    `json:"kubernetes_disabled"`
	InventoryIncomplete     bool                    `json:"inventory_incomplete,omitempty"`
}

type repoMetadata struct {
	Policy PolicyMetadata `json:"repository_policy"`
}

// RepositoryPolicy reads the last successfully ingested policy, never the live checkout.
func RepositoryPolicy(node model.Node) (PolicyMetadata, error) {
	if node.Kind != model.KindRepository {
		return PolicyMetadata{}, errors.New("repository policy requires a repository node")
	}
	if node.Meta == "" {
		return PolicyMetadata{}, nil
	}
	var meta repoMetadata
	if err := json.Unmarshal([]byte(node.Meta), &meta); err != nil {
		return PolicyMetadata{}, fmt.Errorf("decode repository policy: %w", err)
	}
	return meta.Policy, nil
}

func preparePolicy(ctx context.Context, root string, opts Options) (context.Context, error) {
	p := repoconfig.FromContext(ctx)
	if p == nil {
		var err error
		p, err = repoconfig.Load(ctx, root)
		if err != nil {
			return nil, err
		}
	}
	if p.RequiresApproval() && opts.ApprovedPolicyDigest != p.Digest() {
		return nil, ErrPolicyApproval
	}
	return repoconfig.WithPolicy(ctx, p), nil
}

func policyMetadata(ctx context.Context, root string) (PolicyMetadata, error) {
	p := repoconfig.FromContext(ctx)
	cfg := p.Config()
	m := PolicyMetadata{Present: p.Present(), Digest: p.Digest(), Config: cfg, Diagnostics: p.Diagnostics(), ExclusionsApproved: p.RequiresApproval()}
	m.KubernetesDisabled = cfg.Scanners.Kubernetes.Enabled != nil && !*cfg.Scanners.Kubernetes.Enabled
	if !p.RequiresApproval() {
		return m, nil
	}
	// Count directory entries, without reading excluded file contents. A capped
	// inventory is reported as incomplete instead of claiming full coverage.
	count := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			m.InventoryIncomplete = true
			return nil //nolint:nilerr // record unreadable entries as incomplete coverage
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 100000 {
			m.InventoryIncomplete = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			if path != root && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == repoconfig.Filename {
			return nil
		}
		if !p.PathAllowed(rel) {
			m.ExcludedFiles++
			return nil
		}
		ext := strings.ToLower(filepath.Ext(rel))
		if (ext == ".yaml" || ext == ".yml" || ext == ".tpl" || ext == ".tmpl" || ext == ".gotmpl") && !p.KubernetesAllowed(rel) {
			m.KubernetesExcludedFiles++
		}
		return nil
	})
	return m, err
}

func policyCoverage(c security.Coverage, m PolicyMetadata) security.Coverage {
	c.ExcludedFiles = m.ExcludedFiles
	c.KubernetesExcludedFiles = m.KubernetesExcludedFiles
	c.KubernetesDisabled = m.KubernetesDisabled
	c.InventoryIncomplete = m.InventoryIncomplete
	return c
}
