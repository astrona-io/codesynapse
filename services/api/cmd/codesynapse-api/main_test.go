package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestOpenFGADecides(t *testing.T) {
	cfg := &fga.Config{}
	for _, tc := range []struct {
		name, mode string
		api        bool
		cfg        *fga.Config
		want, err  bool
	}{
		{"unset keeps postgres", "", true, cfg, false, false},
		{"postgres", "postgres", true, cfg, false, false},
		{"openfga in the API", "openfga", true, cfg, true, false},
		{"openfga without its configuration", "openfga", true, nil, false, true},
		{"a worker never decides", "openfga", false, nil, false, false},
		{"anything else", "OpenFGA", true, cfg, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := openFGADecides(tc.mode, tc.api, tc.cfg)
			assert.Equal(t, got, tc.want)
			assert.Equal(t, err != nil, tc.err, "error: %v", err)
		})
	}
}

func TestPushAllowed(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name           string
		wanted, worker bool
		roles          []string
		want           bool
	}{
		{"off stays off", false, true, []string{"fix"}, false},
		{"the API keeps the switch to accept fix requests", true, false, []string{}, true},
		{"the API running every job", true, false, nil, true},
		{"a fix worker", true, true, []string{"fix"}, true},
		{"a worker with fix among others", true, true, []string{"ingest", "fix"}, true},
		{"an ingest and ai worker never holds it", true, true, []string{"ingest", "ai"}, false},
		{"a worker with no roles never holds it", true, true, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, pushAllowed(tt.wanted, tt.worker, tt.roles, logger), tt.want)
		})
	}
}
