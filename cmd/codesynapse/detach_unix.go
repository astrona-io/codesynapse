//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the proxy in its own session, so it outlives the hook that
// started it and gets no signals meant for Claude Code's terminal.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
