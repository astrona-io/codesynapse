package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Cursor's MCP SDK filters custom environment variables. A live launcher
// therefore also serves its binding to descendant processes over a private
// Unix socket. The OS parent chain selects the launcher, never a workspace,
// most-recent chat, model argument or arbitrary caller-supplied process ID.
func cursorLaunchDir() string {
	return filepath.Join("/tmp", "codesynapse-launcher-"+strconv.Itoa(os.Getuid()))
}
func cursorSocket(dir string, pid int) string {
	return filepath.Join(dir, "cursor-"+strconv.Itoa(pid)+".sock")
}
func cursorKeyHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func serveCursorLaunch(ctx context.Context, dir, token, id string) (func(), error) {
	if token == "" {
		return nil, errors.New("run codesynapse mcp install with a personal key before launching Cursor")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("cursor launcher directory must be private")
	}
	socket := cursorSocket(dir, os.Getpid())
	// A stale PID socket contains no ID; replace only the socket at our own PID.
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("unexpected Cursor launcher socket path")
		}
		if err := os.Remove(socket); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	hash := cursorKeyHash(token)
	go func() {
		slots := make(chan struct{}, 8)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				_ = conn.Close()
				continue
			}
			go func() {
				defer func() { _ = conn.Close(); <-slots }()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				var request struct {
					KeyHash string `json:"key_hash"`
				}
				if json.NewDecoder(io.LimitReader(conn, 256)).Decode(&request) != nil || subtle.ConstantTimeCompare([]byte(request.KeyHash), []byte(hash)) != 1 {
					return
				}
				_ = json.NewEncoder(conn).Encode(struct {
					ID string `json:"id"`
				}{id})
			}()
		}
	}()
	return func() { _ = listener.Close() }, nil
}

func cursorParent(ctx context.Context, pid int) (int, error) {
	// ps is an OS process query with fixed flags and a numeric PID; no shell.
	out, err := cursorProcessParent(ctx, strconv.Itoa(pid))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

func findCursorLaunch(ctx context.Context, dir, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	// Desktop does not normally have a launcher directory, so avoid process
	// queries on that common path.
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) { //nolint:gosec // fixed private launcher directory derived from the OS user ID
		return "", nil
	} else if err != nil {
		return "", err
	}
	pid := os.Getppid()
	for range 32 {
		if pid <= 1 {
			return "", nil
		}
		socket := cursorSocket(dir, pid)
		if _, err := os.Lstat(socket); err == nil {
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
			if err != nil {
				return "", fmt.Errorf("cursor launcher binding unavailable; restart codesynapse cursor: %w", err)
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if err := json.NewEncoder(conn).Encode(struct {
				KeyHash string `json:"key_hash"`
			}{cursorKeyHash(token)}); err != nil {
				_ = conn.Close()
				return "", err
			}
			var reply struct {
				ID string `json:"id"`
			}
			err = json.NewDecoder(io.LimitReader(conn, 256)).Decode(&reply)
			_ = conn.Close()
			if err != nil {
				return "", errors.New("cursor launcher binding refused; rerun setup and restart the CLI")
			}
			if !validCursorChat(reply.ID) {
				return "", errors.New("invalid Cursor launcher binding")
			}
			return reply.ID, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent, err := cursorParent(ctx, pid)
		if err != nil {
			return "", err
		}
		if parent == pid {
			return "", errors.New("invalid process ancestry")
		}
		pid = parent
	}
	return "", errors.New("cursor launcher ancestry exceeds limit")
}
