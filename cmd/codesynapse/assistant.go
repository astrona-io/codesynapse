package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
)

// These clients already expose their runtime chat identity to the installed
// bridge. Launch without inventing an ID or changing their native arguments.
type assistantLaunchOptions struct {
	EC     *app.Env
	client string
}

func (*assistantLaunchOptions) SetupFlags(context.Context, *app.Env) error { return nil }
func (*assistantLaunchOptions) Complete(context.Context, *app.Env) error   { return nil }
func (*assistantLaunchOptions) Validate(context.Context, *app.Env) error   { return nil }
func (o *assistantLaunchOptions) Run(ctx context.Context, _ *app.Env) error {
	binary, err := exec.LookPath(o.client)
	if err != nil {
		return fmt.Errorf("%s CLI not found; install the official client, then run codesynapse mcp install", o.client)
	}
	args := o.EC.Args
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	command := exec.CommandContext(ctx, binary, args...) //nolint:gosec // fixed client name resolved on PATH; native argv without a shell
	command.Env = os.Environ()
	command.Stdin, command.Stdout, command.Stderr = o.EC.Stdin, o.EC.Stdout, o.EC.Stderr
	return command.Run()
}
func assistantCmd(ec *app.Env, client string) *cobra.Command {
	command := app.NewSubCommand(client+" [native arguments]", &assistantLaunchOptions{EC: ec, client: client}, ec).
		WithShortDesc("Start " + client + " using the installed CodeSynapse integration").
		WithLongDesc("Run codesynapse mcp install once before launching. Native client arguments are forwarded unchanged; use the client's normal resume options to continue a chat.").Build()
	command.DisableFlagParsing = true
	return command
}
