package gitx

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"
)

// HistoryDepth is how many commits clones and fetches keep. Enough for
// churn and history, without downloading entire histories.
const HistoryDepth = 500

// ErrInvalidRemote is returned for remotes CodeSynapse refuses to contact.
var ErrInvalidRemote = errors.New("invalid git remote")

// Auth is an optional token for private repositories on one host.
type Auth struct {
	Host  string // e.g. "github.com"
	Token string
}

// RemoteOptions controls network git operations.
type RemoteOptions struct {
	Auth Auth
	// AllowPrivate permits remotes that resolve to private, loopback or
	// link-local addresses (self-hosted forges on the same network).
	AllowPrivate bool
	// Resolver is used for the SSRF check; nil uses net.DefaultResolver.
	Resolver interface {
		LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	}
}

// ValidateRemote accepts https URLs without embedded credentials whose host
// does not resolve to an internal address (unless AllowPrivate). It returns
// the normalised URL.
func ValidateRemote(ctx context.Context, raw string, opts RemoteOptions) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("%w: only https:// URLs are supported", ErrInvalidRemote)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: credentials in the URL are not allowed; configure CODESYNAPSE_GIT_TOKEN", ErrInvalidRemote)
	}
	if u.RawQuery != "" || u.Fragment != "" || strings.HasPrefix(u.Path, "-") || strings.Contains(u.Path, "..") {
		return "", fmt.Errorf("%w: unexpected characters in URL", ErrInvalidRemote)
	}
	if !opts.AllowPrivate {
		resolver := opts.Resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		addrs, err := resolver.LookupIPAddr(ctx, u.Hostname())
		if err != nil {
			return "", fmt.Errorf("%w: resolving %s: %w", ErrInvalidRemote, u.Hostname(), err)
		}
		for _, a := range addrs {
			if a.IP.IsLoopback() || a.IP.IsPrivate() || a.IP.IsLinkLocalUnicast() || a.IP.IsLinkLocalMulticast() || a.IP.IsUnspecified() {
				return "", fmt.Errorf("%w: %s resolves to an internal address", ErrInvalidRemote, u.Hostname())
			}
		}
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), nil
}

// remoteEnv returns git config overrides for network operations as
// GIT_CONFIG_* environment variables, so tokens never appear in argv.
func remoteEnv(remote string, auth Auth) []string {
	kv := [][2]string{
		{"protocol.allow", "never"},
		{"protocol.https.allow", "always"},
		{"http.followRedirects", "initial"},
		{"core.symlinks", "false"},
		{"submodule.recurse", "false"},
	}
	if auth.Token != "" && auth.Host != "" {
		if u, err := url.Parse(remote); err == nil && strings.EqualFold(u.Hostname(), auth.Host) {
			cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + auth.Token))
			kv = append(kv, [2]string{fmt.Sprintf("http.https://%s/.extraHeader", auth.Host), "Authorization: Basic " + cred})
		}
	}
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(kv))}
	for i, p := range kv {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]))
	}
	return env
}

// runRemote is Run for commands that talk to a remote: the same local
// hardening, but https allowed through environment config.
func runRemote(ctx context.Context, dir, remote string, auth Auth, args ...string) ([]byte, error) {
	full := []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "diff.external="}
	if dir != "" {
		full = append([]string{"-C", dir, "-c", "safe.directory=" + dir}, full...)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...) //nolint:gosec // fixed binary, validated remote and refs
	cmd.Env = append([]string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_ASKPASS=/bin/false",
		"HOME=/nonexistent", "PATH=" + safePath(),
	}, remoteEnv(remote, auth)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// stderr can echo the URL; it never contains the token (header only).
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Clone makes a shallow clone of remote's branch (default branch when empty) into dir.
func Clone(ctx context.Context, remote, branch, dir string, auth Auth) error {
	args := []string{"clone", "--depth", fmt.Sprint(HistoryDepth), "--no-tags", "--single-branch"}
	if branch != "" {
		if err := ValidateRev(branch); err != nil {
			return err
		}
		args = append(args, "--branch", branch)
	}
	_, err := runRemote(ctx, "", remote, auth, append(args, "--", remote, dir)...)
	return err
}

// FetchCheckout fetches branch from origin and checks it out detached,
// discarding any local state. The working tree then matches origin exactly.
func FetchCheckout(ctx context.Context, dir, remote, branch string, auth Auth) error {
	if err := ValidateRev(branch); err != nil {
		return err
	}
	if _, err := runRemote(ctx, dir, remote, auth, "fetch", "--depth", fmt.Sprint(HistoryDepth), "--no-tags", "--force", "origin",
		"+refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return err
	}
	if _, err := Run(ctx, dir, "checkout", "--force", "--detach", "refs/remotes/origin/"+branch); err != nil {
		return err
	}
	_, err := Run(ctx, dir, "clean", "-ffdx")
	return err
}

// RemoteBranches lists branch names on remote.
func RemoteBranches(ctx context.Context, remote string, auth Auth) ([]string, error) {
	out, err := runRemote(ctx, "", remote, auth, "ls-remote", "--heads", "--", remote)
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			branches = append(branches, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	return branches, nil
}

// LocalBranches lists branches of a local repository.
func LocalBranches(ctx context.Context, dir string) ([]string, error) {
	out, err := Run(ctx, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, b := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if b != "" {
			branches = append(branches, b)
		}
	}
	return branches, nil
}

// CurrentBranch returns the checked-out branch, or "" when detached.
func CurrentBranch(ctx context.Context, dir string) string {
	out, err := Run(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// OriginURL returns the origin remote URL, or "".
func OriginURL(ctx context.Context, dir string) string {
	out, err := Run(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// SanitizeURL drops credentials from a remote URL for display and storage.
func SanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	return u.String()
}

// HTTPSRemote rewrites ssh remotes (git@host:owner/repo.git,
// ssh://git@host/owner/repo) to their https form, which is the only scheme
// CodeSynapse clones from. Other URLs are returned unchanged.
func HTTPSRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(raw, "ssh://"); ok {
		if _, after, found := strings.Cut(rest, "@"); found {
			rest = after
		}
		host, p, _ := strings.Cut(rest, "/")
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		return "https://" + host + "/" + p
	}
	if user, rest, ok := strings.Cut(raw, "@"); ok && !strings.Contains(user, "/") && !strings.Contains(raw, "://") {
		if host, p, ok := strings.Cut(rest, ":"); ok {
			return "https://" + host + "/" + strings.TrimPrefix(p, "/")
		}
	}
	return raw
}
