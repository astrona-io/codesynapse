package main

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
)

type repoConfigOptions struct {
	EC   *app.Env
	root string
}

func (*repoConfigOptions) SetupFlags(context.Context, *app.Env) error { return nil }
func (o *repoConfigOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	var err error
	o.root, err = filepath.Abs(o.root)
	return err
}
func (*repoConfigOptions) Validate(context.Context, *app.Env) error { return nil }
func (o *repoConfigOptions) Run(ctx context.Context, _ *app.Env) error {
	policy, err := repoconfig.Load(ctx, o.root)
	if err != nil {
		return err
	}
	out := struct {
		Present          bool                    `json:"present"`
		Digest           string                  `json:"digest"`
		Config           repoconfig.RepoConfig   `json:"config"`
		Diagnostics      []repoconfig.Diagnostic `json:"diagnostics"`
		RequiresApproval bool                    `json:"requires_exclusion_approval"`
	}{policy.Present(), policy.Digest(), policy.Config(), policy.Diagnostics(), policy.RequiresApproval()}
	encoder := json.NewEncoder(o.EC.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}

func repoConfigCmd(ec *app.Env) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect repository configuration", GroupID: app.GroupOther}
	options := &repoConfigOptions{EC: ec}
	cmd.AddCommand(app.NewSubCommand("inspect [path]", options, ec).
		WithShortDesc("Validate repository configuration and print its approval digest as JSON").
		WithExample("  codesynapse config inspect .").WithMaxArgs(1).Build())
	return cmd
}
