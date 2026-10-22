package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestConfigInspect(t *testing.T) {
	for _, tc := range []struct {
		name, text               string
		valid, present, approval bool
	}{
		{"absent", "", true, false, false},
		{"exclusions", "version: 1\nignore:\n  paths: [vendor/**]\n", true, true, true},
		{"invalid", "version: 2\n", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.text != "" {
				require.NoError(t, os.WriteFile(filepath.Join(root, repoconfig.Filename), []byte(tc.text), 0o600))
			}
			var output bytes.Buffer
			ec := &app.Env{Args: []string{root}, Stdout: &output}
			options := &repoConfigOptions{EC: ec}
			require.NoError(t, options.Complete(context.Background(), ec))
			err := options.Run(context.Background(), ec)
			if !tc.valid {
				require.Error(t, err)
				assert.Equal(t, output.Len(), 0)
				return
			}
			require.NoError(t, err)
			var result struct {
				Present  bool
				Digest   string
				Approval bool `json:"requires_exclusion_approval"`
			}
			require.NoError(t, json.Unmarshal(output.Bytes(), &result))
			assert.Equal(t, result.Present, tc.present)
			assert.Equal(t, result.Approval, tc.approval)
			if tc.present {
				policy, err := repoconfig.Load(context.Background(), root)
				require.NoError(t, err)
				assert.Equal(t, result.Digest, policy.Digest())
			}
		})
	}
}

func TestPolicyApprovalFlags(t *testing.T) {
	for _, build := range []func(*app.Env) *cobra.Command{ingestCmd, reviewCmd, statsCmd, auditCmd, licensesCmd, cleanupCmd, duplicatesCmd, sbomCmd} {
		command := build(&app.Env{})
		assert.True(t, command.Flags().Lookup("approved-policy-digest") != nil, command.Name())
	}
}

func TestLicensesRequiresExplicitPolicyApproval(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, repoconfig.Filename), []byte("version: 1\nignore:\n  paths: [excluded/**]\n"), 0o600))
	var output bytes.Buffer
	ec := &app.Env{Stdout: &output, Output: app.OutputJSON, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	options := &licensesOptions{EC: ec, root: root}
	assert.True(t, errors.Is(options.Run(context.Background(), ec), ingest.ErrPolicyApproval))
	policy, err := repoconfig.Load(context.Background(), root)
	require.NoError(t, err)
	options.approvedPolicyDigest = policy.Digest()
	require.NoError(t, options.Run(context.Background(), ec))
	assert.True(t, json.Valid(output.Bytes()))
}
