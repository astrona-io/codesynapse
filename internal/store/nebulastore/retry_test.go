package nebulastore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	nebula "github.com/vesoft-inc/nebula-go/v3"
	graphcodes "github.com/vesoft-inc/nebula-go/v3/nebula"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestRetriable(t *testing.T) {
	exec := int(nebula.ErrorCode_E_EXECUTION_ERROR)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("More than one request trying to add/update/delete one edge/vertex at the same time"), false},
		{"context deadline", context.DeadlineExceeded, false},
		{"memory lock conflict", &Error{Code: exec, Message: "Storage Error: More than one request trying to add/update/delete one edge/vertex at the same time."}, true},
		{"wrapped conflict", fmt.Errorf("reserve ingest: %w", &Error{Code: exec, Message: "Storage Error: More than one request trying to add/update/delete one edge/vertex at the same time."}), true},
		{"generic part error naming the code", &Error{Code: exec, Message: "Storage Error: part: 3, error: E_DATA_CONFLICT_ERROR(-3010)."}, true},
		{"leader changed", &Error{Code: exec, Message: "Storage Error: Not the leader of 2. Please retry later."}, true},
		{"snapshot blocks writes", &Error{Code: exec, Message: "Storage Error: Write is blocked when creating snapshot. Please retry later."}, true},
		{"raft buffer full", &Error{Code: exec, Message: "Storage Error: Part 4 raft buffer is full. Please retry later."}, true},
		{"term out of date may have applied", &Error{Code: exec, Message: "Storage Error: Term of part 1 is out of date. Please retry later."}, false},
		{"rpc timeout may have applied", &Error{Code: exec, Message: "Storage Error: RPC failure, probably timeout."}, false},
		{"filtered upsert", &Error{Code: exec, Message: "Storage Error: Invalid filter."}, false},
		{"wrong type", &Error{Code: exec, Message: "Storage Error: The data type does not meet the requirements. Use the correct type of data."}, false},
		{"syntax error", &Error{Code: int(nebula.ErrorCode_E_SYNTAX_ERROR), Message: "SyntaxError: More than one request"}, false},
		{"permission", &Error{Code: int(nebula.ErrorCode_E_BAD_PERMISSION), Message: "Please retry later"}, false},
		{"data conflict code", &Error{Code: int(graphcodes.ErrorCode_E_DATA_CONFLICT_ERROR)}, true},
		{"leader changed code", &Error{Code: int(graphcodes.ErrorCode_E_LEADER_CHANGED)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, retriable(tc.err), tc.want)
		})
	}
}

func TestRetryWaitIsBounded(t *testing.T) {
	var total time.Duration
	for try := 1; try < writeTries; try++ {
		w := retryWait(try)
		base := time.Duration(try) * 20 * time.Millisecond
		assert.True(t, w >= base && w < 2*base, fmt.Sprintf("try %d waits %s", try, w))
		total += w
	}
	// All retries of one write together stay far below a write's timeout.
	assert.True(t, total < time.Second, total.String())
}
