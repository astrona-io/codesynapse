package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/logx"
)

// Output formats accepted by -o/--output.
const (
	OutputPlain    = "plain"
	OutputMarkdown = "markdown"
	OutputJSON     = "json"
	// OutputSARIF is SARIF 2.1.0, for commands that report findings.
	OutputSARIF = "sarif"
)

// Command groups for help output.
const (
	GroupBase  = "base"
	GroupOther = "other"
)

// Env is shared by every command of a binary: streams, logger, global
// flags and the command currently running.
type Env struct {
	AppName    string
	Version    string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	IsTerminal bool
	Logger     *slog.Logger
	// Output is the value of -o/--output.
	Output string
	// Command is the command being set up or run; Args its positional args.
	Command *cobra.Command
	Args    []string

	logLevel  string
	logFormat string
	level     *slog.LevelVar
}

// Runner is implemented by subcommand option structs. SetupFlags runs at
// build time (Env.Command is the new command); the rest run in order when
// the command executes.
type Runner interface {
	SetupFlags(ctx context.Context, env *Env) error
	Complete(ctx context.Context, env *Env) error
	Validate(ctx context.Context, env *Env) error
	Run(ctx context.Context, env *Env) error
}

// SubCommand builds a cobra command around a Runner.
type SubCommand struct {
	cmd    *cobra.Command
	runner Runner
	env    *Env
}

// NewSubCommand starts building a subcommand.
func NewSubCommand(use string, r Runner, env *Env) *SubCommand {
	return &SubCommand{cmd: &cobra.Command{Use: use}, runner: r, env: env}
}

// WithShortDesc sets the one-line description.
func (b *SubCommand) WithShortDesc(s string) *SubCommand { b.cmd.Short = s; return b }

// WithLongDesc sets the long description.
func (b *SubCommand) WithLongDesc(s string) *SubCommand { b.cmd.Long = s; return b }

// WithExample sets usage examples.
func (b *SubCommand) WithExample(s string) *SubCommand { b.cmd.Example = s; return b }

// WithGroupID puts the command in a help group.
func (b *SubCommand) WithGroupID(id string) *SubCommand { b.cmd.GroupID = id; return b }

// WithNoArgs rejects positional arguments.
func (b *SubCommand) WithNoArgs() *SubCommand { b.cmd.Args = cobra.NoArgs; return b }

// WithMaxArgs accepts at most n positional arguments.
func (b *SubCommand) WithMaxArgs(n int) *SubCommand { b.cmd.Args = cobra.MaximumNArgs(n); return b }

// WithArgs sets the Args validator to accept between minN and maxN arguments.
func (b *SubCommand) WithArgs(minN, maxN int) *SubCommand {
	b.cmd.Args = cobra.RangeArgs(minN, maxN)
	return b
}

// Build finalises the command. Flag setup errors are programmer errors.
func (b *SubCommand) Build() *cobra.Command {
	b.env.Command = b.cmd
	if err := b.runner.SetupFlags(context.Background(), b.env); err != nil {
		panic(fmt.Sprintf("setting up flags for %s: %v", b.cmd.Use, err))
	}
	b.cmd.RunE = func(c *cobra.Command, _ []string) error {
		ctx := c.Context()
		if err := b.runner.Complete(ctx, b.env); err != nil {
			return err
		}
		if err := b.runner.Validate(ctx, b.env); err != nil {
			return err
		}
		return b.runner.Run(ctx, b.env)
	}
	return b.cmd
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Main builds the root command, adds the subcommands from build and runs it
// with a context cancelled on SIGINT/SIGTERM. It returns the exit code.
func Main(name, short, version string, build func(env *Env) []*cobra.Command) int {
	env := &Env{
		AppName: name, Version: version,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		IsTerminal: isTerminal(os.Stdin) && isTerminal(os.Stderr),
		level:      new(slog.LevelVar),
	}
	env.Logger = slog.New(traceHandler{slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: env.level})})

	root := &cobra.Command{
		Use:           name,
		Short:         short,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(c *cobra.Command, args []string) error {
			env.Command, env.Args = c, args
			return env.configureLogger()
		},
	}
	pf := root.PersistentFlags()
	pf.StringVar(&env.logLevel, "log-level", Getenv("CODESYNAPSE_LOG_LEVEL", "info"), "Log level (debug, info, warn, error)")
	pf.StringVar(&env.logFormat, "log-format", Getenv("CODESYNAPSE_LOG_FORMAT", "text"), "Log format (text, json)")
	pf.StringVarP(&env.Output, "output", "o", OutputPlain, "Output format (plain, markdown, json; sarif for audit)")
	root.AddGroup(&cobra.Group{ID: GroupBase, Title: "Commands:"}, &cobra.Group{ID: GroupOther, Title: "Other commands:"})
	root.SetHelpCommandGroupID(GroupOther)
	root.SetCompletionCommandGroupID(GroupOther)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	root.AddCommand(build(env)...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = logx.WithLogger(ctx, env.Logger)
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

func (e *Env) configureLogger() error {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(e.logLevel)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	e.level.Set(lvl)
	switch e.logFormat {
	case "text":
		e.Logger = slog.New(traceHandler{slog.NewTextHandler(e.Stderr, &slog.HandlerOptions{Level: e.level})})
	case "json":
		e.Logger = slog.New(traceHandler{slog.NewJSONHandler(e.Stderr, &slog.HandlerOptions{Level: e.level})})
	default:
		return fmt.Errorf("--log-format must be text or json, not %q", e.logFormat)
	}
	return nil
}

// Ctx returns ctx carrying the environment's logger.
func Ctx(ctx context.Context, env *Env) context.Context {
	return logx.WithLogger(ctx, env.Logger)
}
