package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestImmutableIdentity(t *testing.T) {
	id := Identity{TenantID: "tenant-a", SubjectID: "user-a", SessionID: "chat:01", ParentSessionID: "chat:01"}
	ctx, err := With(context.Background(), id)
	require.NoError(t, err)
	id.SessionID = "changed"
	got, err := Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, got.SessionID, "chat:01")
	again, err := With(ctx, got)
	require.NoError(t, err)
	assert.True(t, again == ctx)
	for _, field := range []string{"tenant", "subject", "session"} {
		changed := got
		switch field {
		case "tenant":
			changed.TenantID = "tenant-b"
		case "subject":
			changed.SubjectID = "user-b"
		case "session":
			changed.SessionID, changed.ParentSessionID = "chat-b", "chat-b"
		}
		_, err := With(ctx, changed)
		assert.True(t, errors.Is(err, ErrConflict))
	}
	data, err := json.Marshal(got)
	require.NoError(t, err)
	var child Identity
	require.NoError(t, json.Unmarshal(data, &child))
	assert.Equal(t, child, got)
	childCtx, err := With(context.Background(), child)
	require.NoError(t, err)
	child, err = Require(childCtx)
	require.NoError(t, err)
	assert.Equal(t, child.ParentSessionID, got.SessionID)
}

func TestInvalidIdentity(t *testing.T) {
	for _, value := range []string{"", "has space", "a/b", "a|b", "\r\nInjected", "界", strings.Repeat("a", 129)} {
		id := Identity{TenantID: "org", SubjectID: "user", SessionID: value, ParentSessionID: value}
		assert.True(t, errors.Is(Validate(id), ErrInvalid))
	}
	for _, id := range []Identity{{TenantID: "org", SubjectID: "user", SessionID: "chat"}, {TenantID: "org", SubjectID: "user", SessionID: "chat", ParentSessionID: "other"}, {SubjectID: "user", SessionID: "chat", ParentSessionID: "chat"}} {
		assert.True(t, errors.Is(Validate(id), ErrInvalid))
	}
	_, err := Require(context.Background())
	assert.True(t, errors.Is(err, ErrMissing))
}

func TestConcurrentContextsDoNotMix(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			session := fmt.Sprintf("runtime-%d", i)
			id := Identity{TenantID: "org", SubjectID: "user", SessionID: session, ParentSessionID: session}
			ctx, err := With(context.Background(), id)
			assert.NoError(t, err)
			got, err := Require(context.WithoutCancel(ctx))
			assert.NoError(t, err)
			assert.Equal(t, got, id)
		})
	}
	wg.Wait()
}

func TestRequireRejectsEndedExecution(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			id := Identity{TenantID: "org", SubjectID: "user", SessionID: "thread", ParentSessionID: "thread"}
			ctx, err := With(t.Context(), id)
			require.NoError(t, err)
			var cancel context.CancelFunc
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(ctx)
			}
			cancel()
			got, err := Require(ctx)
			assert.True(t, errors.Is(err, want))
			assert.Equal(t, got, Identity{})
			inherited, ok := From(ctx)
			assert.True(t, ok)
			assert.Equal(t, inherited, id, "cancellation does not change the parent identity")
		})
	}
}
