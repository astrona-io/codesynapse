//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package config

import (
	"os"
	"syscall"
)

func openConfig(root *os.Root) (*os.File, error) {
	// Nonblocking open prevents a concurrent replacement with a FIFO from hanging ingestion.
	return root.OpenFile(Filename, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
