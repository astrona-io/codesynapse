package main

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestReviewFailOnNewNeedsTheBase(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		o    reviewOptions
		ok   bool
	}{
		{"offline with a base", reviewOptions{failOnNew: "high", offline: true, findingsDelta: true}, true},
		{"unknown severity", reviewOptions{failOnNew: "severe", offline: true, findingsDelta: true}, false},
		{"not offline", reviewOptions{failOnNew: "high", findingsDelta: true}, false},
		{"a diff file has no base", reviewOptions{failOnNew: "high", offline: true, findingsDelta: true, diffFile: "x.diff"}, false},
		{"delta turned off", reviewOptions{failOnNew: "high", offline: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.o.EC = &app.Env{Output: app.OutputMarkdown}
			err := tt.o.Validate(ctx, tt.o.EC)
			assert.Equal(t, err == nil, tt.ok)
		})
	}
}
