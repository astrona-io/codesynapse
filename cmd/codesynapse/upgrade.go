package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/upgrade"
)

type upgradeOptions struct {
	EC     *app.Env
	asJSON bool
	module string
	to     string
	root   string
}

func (o *upgradeOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().BoolVar(&o.asJSON, "json", false, "Print the report as JSON")
	return nil
}

func (o *upgradeOptions) Complete(_ context.Context, _ *app.Env) error {
	o.module, o.to, _ = strings.Cut(o.EC.Args[0], "@")
	o.root = "."
	if len(o.EC.Args) > 1 {
		o.root = o.EC.Args[1]
	}
	abs, err := filepath.Abs(o.root)
	o.root = abs
	return err
}

func (o *upgradeOptions) Validate(context.Context, *app.Env) error {
	if o.module == "" {
		return fmt.Errorf("give the module as path or path@version")
	}
	return nil
}

func (o *upgradeOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	r, err := upgrade.Analyze(ctx, upgrade.Options{Root: o.root, Module: o.module, To: o.to})
	if err != nil {
		return err
	}
	if o.asJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	return upgrade.WriteText(o.EC.Stdout, r)
}

func upgradeCmd(ec *app.Env) *cobra.Command {
	return app.NewSubCommand("upgrade-impact module[@version] [path]", &upgradeOptions{EC: ec}, ec).
		WithShortDesc("Show where a Go module is used and what upgrading it changes for this repository (offline)").
		WithLongDesc("Lists every place the repository uses the module's API: calls, types, constants, variables, methods and struct fields. With @version it also compares the exported API of the current version with that one, read from the local module cache, and names the declarations this repository uses that were removed or changed. Nothing is downloaded: `go mod download module@version` fetches a version that is not in the cache. A new major version is a different module path (…/v2) and is not compared.").
		WithExample("  codesynapse upgrade-impact github.com/spf13/cobra@v1.11.0\n  codesynapse upgrade-impact golang.org/x/tools . --json").
		WithGroupID(app.GroupBase).
		WithArgs(1, 2).
		Build()
}
