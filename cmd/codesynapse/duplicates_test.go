package main

import (
	"context"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/duplicates"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestDuplicatesGate(t *testing.T) {
	reports := map[string]duplicates.Report{
		"api":    {Summary: duplicates.Summary{Percent: 7.5}},
		"worker": {Summary: duplicates.Summary{Percent: 2}},
	}
	names := []string{"api", "worker"}

	assert.NoError(t, (&duplicatesOptions{}).gate(names, reports), "off by default")
	assert.NoError(t, (&duplicatesOptions{maxPercent: 10}).gate(names, reports), "under the limit")

	err := (&duplicatesOptions{maxPercent: 5}).gate(names, reports)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api repeats 7.5%")
	assert.False(t, strings.Contains(err.Error(), "worker"), "only repositories over the limit are named")

	assert.Error(t, (&duplicatesOptions{maxPercent: 101}).Validate(context.Background(), nil))
	assert.Error(t, (&duplicatesOptions{maxPercent: -1}).Validate(context.Background(), nil))
	assert.NoError(t, (&duplicatesOptions{maxPercent: 5}).Validate(context.Background(), nil))
}
