package main

import (
	"context"
	"os/exec"
	"time"
)

func cursorProcessParent(ctx context.Context, pid string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "/bin/ps", "-o", "ppid=", "-p", pid).Output() //nolint:gosec // fixed OS binary and flags; PID comes from numeric OS process ancestry
}
