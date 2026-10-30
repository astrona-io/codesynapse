//go:build !unix

package main

import "os/exec"

// detach does nothing where there are no sessions; releases ship for unix.
func detach(*exec.Cmd) {}
