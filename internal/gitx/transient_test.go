package gitx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dns", errors.New("git clone: exit status 128: fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com (Timeout while contacting DNS servers)"), true},
		{"tls", errors.New("git clone: exit status 128: fatal: unable to access 'https://github.com/o/r.git/': TLS connect error: error:0A000126:SSL routines::unexpected eof while reading"), true},
		{"http2", errors.New("git clone: exit status 128: fatal: unable to access 'https://github.com/o/r.git/': Error in the HTTP2 framing layer"), true},
		{"server error", errors.New("git fetch: exit status 128: fatal: unable to access 'https://h/o/r.git/': The requested URL returned error: 503"), true},
		{"hung up", errors.New("git clone: exit status 128: fatal: the remote end hung up unexpectedly"), true},
		{"denied", fmt.Errorf("git clone: %w: could not resolve host", ErrNoAccess), false},
		{"redirect", fmt.Errorf("git clone: %w", ErrRedirected), false},
		{"not found", errors.New("git clone: exit status 128: fatal: repository 'https://h/o/r.git/' not found"), false},
		{"cancelled", fmt.Errorf("git clone: %w", context.Canceled), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, IsTransient(tc.err), tc.want)
		})
	}
}
