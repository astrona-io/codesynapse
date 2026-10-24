package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCursorLauncherUsesRuntimeChat(t *testing.T) {
	t.Setenv(apiTokenEnv, "csk_test")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cursor-agent"), []byte("#!/bin/sh\nif [ \"$1\" = create-chat ]; then echo native-chat; exit; fi\nprintf '%s|%s|%s' \"$1\" \"$2\" \"$CODESYNAPSE_CURSOR_CLI_SESSION\"\n"), 0o700))
	t.Setenv("PATH", dir)
	for _, resume := range []string{"", "existing-chat"} {
		t.Run(resume, func(t *testing.T) {
			var out bytes.Buffer
			env := &app.Env{Stdout: &out, Stderr: &out}
			o := cursorLaunchOptions{EC: env, resume: resume}
			require.NoError(t, o.Validate(t.Context(), env))
			require.NoError(t, o.Run(t.Context(), env))
			id := resume
			if id == "" {
				id = "native-chat"
			}
			assert.Equal(t, out.String(), "--resume|"+id+"|"+id)
		})
	}
}
func TestCursorLauncherRejectsIdentityOverrides(t *testing.T) {
	for _, arg := range []string{"--resume", "--resume=other", "--continue", "--new-session-id=fake"} {
		env := &app.Env{Args: []string{arg}}
		o := cursorLaunchOptions{EC: env}
		assert.Error(t, o.Validate(context.Background(), env))
	}
}
func TestCursorLauncherRejectsMalformedRuntimeOutput(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cursor-agent"), []byte("#!/bin/sh\necho 'not a chat id'\n"), 0o700))
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	o := cursorLaunchOptions{EC: &app.Env{Stdout: &out, Stderr: &out}}
	assert.Error(t, o.Run(t.Context(), o.EC))
}

func TestCursorCLIChatSwitchRefusesCalls(t *testing.T) {
	t.Setenv("CODESYNAPSE_RUNTIME_DIR", t.TempDir())
	api := apiClient{base: "https://example.test", token: "csk_test"}
	routes := runtimeRoutes{client: "cursor", primary: "native-chat", api: api}
	id, err := routes.identity([]byte(`{"params":{}}`))
	require.NoError(t, err)
	assert.Equal(t, id, "native-chat")
	require.NoError(t, writeRuntimeReceipt(api, "cursor", "thread", "cli-mismatch:native-chat", "another-chat"))
	_, err = routes.identity([]byte(`{"params":{}}`))
	assert.Error(t, err)
}
