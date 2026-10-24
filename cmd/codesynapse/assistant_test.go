package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestAssistantLaunchersForwardNativeArguments(t *testing.T) {
	dir := t.TempDir()
	for _, client := range []string{"codex", "claude"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, client), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700))
	}
	t.Setenv("PATH", dir)
	for _, client := range []string{"codex", "claude"} {
		for _, args := range [][]string{{"--model", "native model", "--resume", "runtime-id"}, {"--", "--continue"}} {
			t.Run(client+args[0], func(t *testing.T) {
				var out bytes.Buffer
				env := &app.Env{Stdout: &out, Stderr: &out}
				root := &cobra.Command{Use: "codesynapse", PersistentPreRun: func(c *cobra.Command, args []string) { env.Command = c; env.Args = args }}
				root.AddCommand(assistantCmd(env, client))
				root.SetArgs(append([]string{client}, args...))
				require.NoError(t, root.ExecuteContext(t.Context()))
				if args[0] == "--" {
					assert.Equal(t, out.String(), "--continue\n")
				} else {
					assert.Equal(t, out.String(), "--model\nnative model\n--resume\nruntime-id\n")
				}
			})
		}
	}
}
func TestAssistantLauncherMissingClient(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	env := &app.Env{}
	o := assistantLaunchOptions{EC: env, client: "codex"}
	assert.Error(t, o.Run(t.Context(), env))
}
