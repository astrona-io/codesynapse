package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

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
