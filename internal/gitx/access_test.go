package gitx

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestDeniedMessages(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		denied bool
	}{
		{"remote: Repository not found.\nfatal: repository 'https://github.com/acme/secret.git/' not found", true},
		{"fatal: Authentication failed for 'https://gitlab.com/acme/x.git/'", true},
		{"fatal: could not read Username for 'https://github.com': terminal prompts disabled", true},
		{"fatal: unable to access 'https://git.example.com/x.git/': The requested URL returned error: 403", true},
		{"fatal: unable to access 'https://github.com/x.git/': Could not resolve host: github.com", false},
		{"fatal: unable to access 'https://github.com/x.git/': Failed to connect to github.com port 443: Connection timed out", false},
		{"fatal: unable to access 'https://github.com/x.git/': The requested URL returned error: 502", false},
	} {
		assert.Equal(t, deniedRe.MatchString(tc.stderr), tc.denied, tc.stderr)
	}
}
