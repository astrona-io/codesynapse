// Package ghtools answers the questions developers put to the GitHub CLI as
// small, deterministic JSON instead of raw command output: where a pull
// request stands (PRStatus, with its checks counted and only the ones that
// are not passing named) and which pull requests are open (PRList). An agent
// that would otherwise run gh pr checks or gh pr view and read dozens of
// lines of names and URLs gets the same facts in a few lines.
//
// Everything is read-only and runs on the developer's machine with their own
// gh sign-in; the server never sees a GitHub token. gh is started without a
// shell, with a fixed argument list (only validated numbers and enums vary),
// prompts and colour off, and its output capped. The repository is the one
// the checkout's remote names. The answers hold no URLs, no pull request
// bodies and no e-mail addresses; titles and check names pass
// redact.Summary.
package ghtools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/astrona-io/codesynapse/internal/redact"
)

// Limits on every answer and on gh itself.
const (
	MaxNumber    = 10_000_000
	MaxList      = 30
	defaultList  = 10
	MaxNames     = 15 // check names listed per group
	maxGHOutput  = 4 << 20
	maxLogBytes  = 8 << 20 // job log read from gh; the answer is far smaller
	titleLimit   = 120
	nameLimit    = 80
	messageLimit = 200
)

// ErrInvalid reports an argument the tools refuse.
var ErrInvalid = errors.New("invalid argument")

// ErrUnavailable reports that gh cannot answer: it is missing, not signed in,
// or the checkout has no pull request or GitHub remote to ask about.
var ErrUnavailable = errors.New("gh unavailable")

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func safeText(s string, n int) string { return redact.Summary(clip(strings.TrimSpace(s), n), n) }

// gh runs the GitHub CLI in dir. It is a variable so tests can stand in for
// the binary without touching the network.
var gh = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	out, over, err := runGH(ctx, dir, maxGHOutput, args)
	if err == nil && over {
		return nil, fmt.Errorf("%w: answer larger than %d bytes", ErrUnavailable, maxGHOutput)
	}
	return out, err
}

// ghLog is gh for a job log, which can be huge: it keeps the first
// maxLogBytes and says when it cut the rest.
func ghLog(ctx context.Context, dir string, args ...string) (out []byte, truncated bool, err error) {
	return runGH(ctx, dir, maxLogBytes, args)
}

func runGH(ctx context.Context, dir string, limit int, args []string) ([]byte, bool, error) {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return nil, false, fmt.Errorf("%w: the GitHub CLI (gh) is not installed", ErrUnavailable)
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // a looked-up binary with a fixed argument list
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "NO_COLOR=1", "GH_PAGER=cat", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1", "GH_FORCE_TTY=")
	var out, errb limitedBuffer
	out.max, errb.max = limit, 4096
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		// Reading stops once the cap is hit, so a killed pipe is not a failure.
		if !out.over {
			msg, _, _ := strings.Cut(strings.TrimSpace(errb.String()), "\n")
			return nil, false, fmt.Errorf("%w: %s", ErrUnavailable, safeText(msg, messageLimit))
		}
	}
	return out.Bytes(), out.over, nil
}

// limitedBuffer keeps at most max bytes and notes when more arrived.
type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.max - b.Len(); n > room {
		b.over = true
		p = p[:max(room, 0)]
	}
	b.Buffer.Write(p) //nolint:errcheck // a bytes.Buffer cannot fail
	return n, nil     // never a short write: exec would stop copying
}
