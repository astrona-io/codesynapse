package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCursorLauncherIPC(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cs-ipc-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	stop, err := serveCursorLaunch(t.Context(), dir, "csk_test", "actual-runtime-chat")
	require.NoError(t, err)
	defer stop()
	for _, token := range []string{"csk_test", "csk_other"} {
		// Deliberately discard the process-only session variable, as Cursor's
		// MCP SDK does. The child must resolve its live launcher through ancestry.
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCursorLauncherIPCChild$", "-test.v")
		command.Env = []string{"CS_TEST_LAUNCHER_DIR=" + dir, "CS_TEST_KEY=" + token}
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		if token == "csk_test" {
			assert.True(t, strings.Contains(string(output), "actual-runtime-chat"))
		} else {
			assert.True(t, strings.Contains(string(output), "refused"))
		}
	}
	stop()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCursorLauncherIPCChild$", "-test.v")
	command.Env = []string{"CS_TEST_LAUNCHER_DIR=" + dir, "CS_TEST_KEY=csk_test"}
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.False(t, strings.Contains(string(output), "actual-runtime-chat"))
}
func TestCursorLauncherIPCChild(t *testing.T) {
	dir := os.Getenv("CS_TEST_LAUNCHER_DIR")
	if dir == "" {
		return
	}
	id, err := findCursorLaunch(t.Context(), dir, os.Getenv("CS_TEST_KEY"))
	if err != nil {
		t.Log("refused")
		return
	}
	t.Log(id)
}
