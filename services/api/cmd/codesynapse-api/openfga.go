package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/authz/fgasync"
)

// maxVerifyOrgs bounds what one openfga-verify run compares.
const maxVerifyOrgs = 1000

type openFGARebuildOptions struct {
	EC  *app.Env
	org string
}

func (o *openFGARebuildOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.org, "org", "", "Organisation ID to rebuild (default: every organisation)")
	return nil
}

func (o *openFGARebuildOptions) Complete(context.Context, *app.Env) error { return nil }

func (o *openFGARebuildOptions) Validate(context.Context, *app.Env) error {
	if os.Getenv(EnvDatabaseURL) == "" {
		return fmt.Errorf("%s is required", EnvDatabaseURL)
	}
	return nil
}

// Run marks organisations for a full rebuild (audited). It writes nothing
// to OpenFGA: the API process holding the api-loops lease does, on its
// next pass.
func (o *openFGARebuildOptions) Run(ctx context.Context, _ *app.Env) error {
	svc, closeDB, err := openAccounts(ctx, o.EC.Logger)
	if err != nil {
		return err
	}
	defer closeDB()
	n, err := svc.OperatorOpenFGARebuild(ctx, o.org)
	if err != nil {
		return err
	}
	o.EC.Logger.Info("openfga rebuild requested", "organisations", n)
	return nil
}

func openFGARebuildCmd(ec *app.Env) *cobra.Command {
	o := &openFGARebuildOptions{EC: ec}
	return app.NewSubCommand("openfga-rebuild", o, ec).
		WithShortDesc("Have the API compare organisations' access policy with OpenFGA and repair the drift").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
}

type openFGAVerifyOptions struct {
	EC  *app.Env
	org string
	cfg fga.Config
}

func (o *openFGAVerifyOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.org, "org", "", "Organisation ID to compare (default: every organisation)")
	return nil
}

func (o *openFGAVerifyOptions) Complete(context.Context, *app.Env) error { return nil }

func (o *openFGAVerifyOptions) Validate(context.Context, *app.Env) error {
	if os.Getenv(EnvDatabaseURL) == "" {
		return fmt.Errorf("%s is required", EnvDatabaseURL)
	}
	cfg, err := fga.ConfigFromEnv()
	if err != nil {
		return err
	}
	o.cfg = cfg
	return nil
}

// Run compares PostgreSQL's policy with OpenFGA's tuples and prints the
// drift per organisation as JSON lines. It writes no tuples; the comparison
// is audited (openfga.verify) before it starts.
func (o *openFGAVerifyOptions) Run(ctx context.Context, _ *app.Env) error {
	svc, closeDB, err := openAccounts(ctx, o.EC.Logger)
	if err != nil {
		return err
	}
	defer closeDB()
	if svc == nil {
		return errors.New("accounts are not configured")
	}
	records, err := svc.OpenFGARecords()
	if err != nil {
		return err
	}
	client, err := fga.New(o.cfg)
	if err != nil {
		return err
	}
	if _, err := client.EnsureStore(ctx, "codesynapse"); err != nil {
		return err
	}
	if _, err := client.EnsureModel(ctx); err != nil {
		return err
	}
	orgs := []string{o.org}
	if o.org == "" {
		all, err := svc.St.ListOrgs(ctx, maxVerifyOrgs)
		if err != nil {
			return err
		}
		orgs = orgs[:0]
		for _, org := range all {
			orgs = append(orgs, org.ID)
		}
	}
	if _, err := svc.Record(ctx, accounts.Event{Via: "operator", Action: "openfga.verify", OrgID: o.org, Target: o.org}); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	syncer := &fgasync.Syncer{Client: client, Records: records, Policy: svc.OrgPolicy}
	enc := json.NewEncoder(os.Stdout)
	drift := 0
	for _, id := range orgs {
		res, err := syncer.Verify(ctx, id)
		if err != nil {
			return fmt.Errorf("verify %s: %w", id, err)
		}
		if res.Drift() {
			drift++
		}
		if err := enc.Encode(res); err != nil {
			return err
		}
	}
	if drift > 0 {
		return fmt.Errorf("%d of %d organisations differ from OpenFGA (codesynapse-api openfga-rebuild repairs them)", drift, len(orgs))
	}
	return nil
}

func openFGAVerifyCmd(ec *app.Env) *cobra.Command {
	o := &openFGAVerifyOptions{EC: ec}
	return app.NewSubCommand("openfga-verify", o, ec).
		WithShortDesc("Compare organisations' access policy with OpenFGA's tuples without changing them").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
}
