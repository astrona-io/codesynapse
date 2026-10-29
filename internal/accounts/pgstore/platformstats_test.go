package pgstore

import (
	"strconv"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestStatsRowCap(t *testing.T) {
	assert.Equal(t, maxStatsRowsConst, strconv.Itoa(accounts.MaxStatsRows))
}
