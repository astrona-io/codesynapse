//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package config

import (
	"errors"
	"os"
)

func openConfig(_ *os.Root) (*os.File, error) {
	return nil, errors.New("repository configuration requires a platform with no-follow, nonblocking file open")
}
