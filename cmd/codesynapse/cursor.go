package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
)

const cursorCLISessionEnv = "CODESYNAPSE_CURSOR_CLI_SESSION"

type cursorLaunchOptions struct {
	EC     *app.Env
	resume string
}

func (o *cursorLaunchOptions) SetupFlags(context.Context, *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.resume, "resume", "", "Resume an existing Cursor chat ID instead of creating a new chat")
	return nil
}
func (o *cursorLaunchOptions) Complete(context.Context, *app.Env) error { return nil }
func (o *cursorLaunchOptions) Validate(context.Context, *app.Env) error {
	if o.resume != "" && execution.Validate(execution.Identity{TenantID: "t", SubjectID: "u", SessionID: o.resume, ParentSessionID: o.resume}) != nil {
		return execution.ErrInvalid
	}
	if len(o.EC.Args) > 0 && o.EC.Args[0] == "resume" {
		return errors.New("use codesynapse cursor --resume ID to select the runtime chat")
	}
	for _, arg := range o.EC.Args {
		for _, flag := range []string{"--resume", "--continue", "--new-session-id"} {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return errors.New("the launcher owns Cursor session flags; use codesynapse cursor --resume ID before --")
			}
		}
	}
	return nil
}

// The runtime creates its own chat. The launcher never manufactures a primary
// ID, and only this child process inherits it; the desktop's shared MCP
// process continues to use per-call hook correlation.
func (o *cursorLaunchOptions) Run(ctx context.Context, _ *app.Env) error {
	binary, err := exec.LookPath("cursor-agent")
	if err != nil {
		return errors.New("cursor CLI not found; install the official Cursor CLI first")
	}
	id := o.resume
	if id == "" {
		createCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		command := exec.CommandContext(createCtx, binary, "create-chat") //nolint:gosec // trusted installed CLI, fixed argv
		var output cursorChatOutput
		command.Stdout = &output
		command.Stderr = o.EC.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("cursor could not create a runtime chat: %w", err)
		}
		id = strings.TrimSpace(string(output.data))
		if output.truncated || execution.Validate(execution.Identity{TenantID: "t", SubjectID: "u", SessionID: id, ParentSessionID: id}) != nil {
			return errors.New("cursor create-chat did not return one valid runtime ID; update Cursor CLI")
		}
	}
	stop, err := serveCursorLaunch(ctx, cursorLaunchDir(), newAPIClient("").token, id)
	if err != nil {
		return err
	}
	defer stop()
	args := append([]string{"--resume", id}, o.EC.Args...)
	command := exec.CommandContext(ctx, binary, args...) //nolint:gosec // no shell; session flags validated above
	command.Env = append(os.Environ(), cursorCLISessionEnv+"="+id)
	command.Stdin, command.Stdout, command.Stderr = o.EC.Stdin, o.EC.Stdout, o.EC.Stderr
	return command.Run()
}

type cursorChatOutput struct {
	data      []byte
	truncated bool
}

func (b *cursorChatOutput) Write(p []byte) (int, error) {
	n := min(len(p), 4096-len(b.data))
	b.data = append(b.data, p[:n]...)
	if n < len(p) {
		b.truncated = true
	}
	return len(p), nil
}

func cursorCmd(ec *app.Env) *cobra.Command {
	return app.NewSubCommand("cursor [-- Cursor arguments]", &cursorLaunchOptions{EC: ec}, ec).
		WithShortDesc("Start Cursor CLI with automatic per-chat MCP identity").
		WithExample("  codesynapse cursor\n  codesynapse cursor -- --model auto\n  codesynapse cursor --resume <existing-chat-id>").Build()
}

func validCursorChat(id string) bool {
	return execution.Validate(execution.Identity{TenantID: "t", SubjectID: "u", SessionID: id, ParentSessionID: id}) == nil
}
