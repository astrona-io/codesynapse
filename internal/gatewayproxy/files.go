// Package gatewayproxy is the developer's local failover proxy in front of
// the CodeSynapse agent gateway. Claude Code sends its model requests to the
// proxy on 127.0.0.1; the proxy forwards them to the gateway and, only while
// the gateway cannot be reached, sends them straight to Anthropic with the
// developer's own credential. Those direct requests are counted (never their
// text) and reported to the gateway once it answers again.
package gatewayproxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
)

// DefaultPort is where the proxy listens unless install chose another.
const DefaultPort = 18431

// Config is what `mcp install --gateway` saves for the proxy: the real
// gateway, the loopback port Claude Code is pointed at and the SHA-256 of
// the key install wrote into Claude Code's settings. The key itself is not
// saved: the proxy takes it from the requests Claude Code sends, and only a
// key with this digest.
type Config struct {
	Gateway string `json:"gateway"`
	Port    int    `json:"port"`
	KeyHash string `json:"key_sha256,omitempty"`
}

// HashKey is the digest a Config pins its key by.
func HashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

var keyHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// URL is the base URL Claude Code uses to reach the proxy.
func (c Config) URL() string { return "http://127.0.0.1:" + strconv.Itoa(c.Port) }

// Files are the proxy's files in the CLI's user config folder, all 0600.
type Files struct{ Dir string }

// DefaultFiles uses os.UserConfigDir()/codesynapse, beside the API token.
func DefaultFiles() (Files, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Files{}, err
	}
	return Files{Dir: filepath.Join(dir, "codesynapse")}, nil
}

func (f Files) configPath() string { return filepath.Join(f.Dir, "gateway-proxy.json") }
func (f Files) secretPath() string { return filepath.Join(f.Dir, "gateway-proxy.secret") }
func (f Files) countsPath() string { return filepath.Join(f.Dir, "gateway-failover.json") }
func (f Files) directPath() string { return filepath.Join(f.Dir, "gateway-direct") }

// LogPath is where a proxy started in the background writes its log
// (events only, never requests).
func (f Files) LogPath() string { return filepath.Join(f.Dir, "gateway-proxy.log") }

// ErrNotConfigured means install never set the proxy up (or --remove ran).
var ErrNotConfigured = errors.New("the gateway proxy is not configured")

// maxSmallFile bounds the config and counts files read back.
const maxSmallFile = 64 << 10

func readSmall(p string) ([]byte, error) {
	fh, err := os.Open(p) //nolint:gosec // the CLI's own config folder
	if err != nil {
		return nil, err
	}
	defer fh.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(io.LimitReader(fh, maxSmallFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSmallFile {
		return nil, fmt.Errorf("%s is too large", p)
	}
	return b, nil
}

// writePrivate replaces p atomically with a 0600 file.
func writePrivate(p string, data []byte) error { return agentcfg.WritePrivate(p, data) }

// Load reads the saved config and checks it again: the file is the user's
// to edit, so the gateway URL passes the same rules as at install.
func (f Files) Load() (Config, error) {
	b, err := readSmall(f.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNotConfigured
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", f.configPath(), err)
	}
	return c, c.Validate()
}

// Validate checks the gateway URL like install does and the port range.
func (c Config) Validate() error {
	if err := agentcfg.ValidGatewayBase(c.Gateway); err != nil {
		return err
	}
	if c.Port < 1024 || c.Port > 65535 {
		return fmt.Errorf("proxy port %d is outside 1024-65535", c.Port)
	}
	if c.KeyHash != "" && !keyHashRe.MatchString(c.KeyHash) {
		return errors.New("the proxy config's key digest is not a SHA-256")
	}
	return nil
}

// Save writes the config (0600).
func (f Files) Save(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivate(f.configPath(), append(b, '\n')); err != nil {
		return err
	}
	if err := os.Remove(f.directPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ChooseDirect records that install pointed Claude Code at the gateway
// without failover on purpose (--failover=false), so doctor leaves that
// setup alone instead of taking it for one made before failover existed.
// Save and Remove clear it.
func (f Files) ChooseDirect() error {
	return writePrivate(f.directPath(), []byte("failover=false\n"))
}

// DirectChosen says whether ChooseDirect recorded the choice.
func (f Files) DirectChosen() bool {
	_, err := os.Stat(f.directPath())
	return err == nil
}

// Secret returns the per-user secret the health endpoint proves it holds,
// creating it on first use. It never leaves this machine.
func (f Files) Secret() ([]byte, error) {
	b, err := readSmall(f.secretPath())
	if err == nil && len(b) == secretLen {
		return b, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	b = make([]byte, secretLen)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, writePrivate(f.secretPath(), b)
}

const secretLen = 32

// Remove deletes the config, the secret and any counts not yet reported, and
// says how many direct requests were never reported. A running proxy sees its
// config gone and stops (Proxy.Run).
func (f Files) Remove() (unreported int, err error) {
	if w, lerr := loadWindows(f.countsPath()); lerr == nil {
		for _, x := range w {
			unreported += x.Requests
		}
	}
	for _, p := range []string{f.configPath(), f.secretPath(), f.countsPath(), f.LogPath(), f.directPath()} {
		if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
	}
	return unreported, err
}
