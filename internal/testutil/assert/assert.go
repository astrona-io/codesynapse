// Package assert provides test assertions that mark the test failed and continue.
package assert

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"cmp"

	"github.com/parisnakitakejser/codesynapse/internal/testutil"
)

func fail(t testing.TB, msg string, msgAndArgs []any) bool {
	t.Helper()
	if extra := testutil.Message(msgAndArgs); extra != "" {
		msg += ": " + extra
	}
	t.Errorf("%s", msg)
	return false
}

// Equal checks got == want (deep equality).
func Equal[T any](t testing.TB, got, want T, msgAndArgs ...any) bool {
	t.Helper()
	if testutil.Equal(got, want) {
		return true
	}
	return fail(t, sprintf("not equal\n  got:  %#v\n  want: %#v", got, want), msgAndArgs)
}

// NotEqual checks got != want.
func NotEqual[T any](t testing.TB, got, unwanted T, msgAndArgs ...any) bool {
	t.Helper()
	if !testutil.Equal(got, unwanted) {
		return true
	}
	return fail(t, sprintf("unexpectedly equal to %#v", got), msgAndArgs)
}

// True checks cond.
func True(t testing.TB, cond bool, msgAndArgs ...any) bool {
	t.Helper()
	if cond {
		return true
	}
	return fail(t, "expected true", msgAndArgs)
}

// False checks !cond.
func False(t testing.TB, cond bool, msgAndArgs ...any) bool {
	t.Helper()
	if !cond {
		return true
	}
	return fail(t, "expected false", msgAndArgs)
}

// NoError checks err == nil.
func NoError(t testing.TB, err error, msgAndArgs ...any) bool {
	t.Helper()
	if err == nil {
		return true
	}
	return fail(t, "unexpected error: "+err.Error(), msgAndArgs)
}

// Error checks err != nil.
func Error(t testing.TB, err error, msgAndArgs ...any) bool {
	t.Helper()
	if err != nil {
		return true
	}
	return fail(t, "expected an error", msgAndArgs)
}

// ErrorIs checks errors.Is(err, target).
func ErrorIs(t testing.TB, err, target error, msgAndArgs ...any) bool {
	t.Helper()
	if errors.Is(err, target) {
		return true
	}
	return fail(t, sprintf("error %v is not %v", err, target), msgAndArgs)
}

// Contains checks that haystack (string, slice, array or map) contains needle.
func Contains(t testing.TB, haystack, needle any, msgAndArgs ...any) bool {
	t.Helper()
	if testutil.Contains(haystack, needle) {
		return true
	}
	return fail(t, sprintf("%#v does not contain %#v", haystack, needle), msgAndArgs)
}

// NotContains checks that haystack does not contain needle.
func NotContains(t testing.TB, haystack, needle any, msgAndArgs ...any) bool {
	t.Helper()
	if !testutil.Contains(haystack, needle) {
		return true
	}
	return fail(t, sprintf("%#v contains %#v", haystack, needle), msgAndArgs)
}

// Len checks the length of v.
func Len(t testing.TB, v any, n int, msgAndArgs ...any) bool {
	t.Helper()
	l := testutil.Len(v)
	if l == n {
		return true
	}
	return fail(t, sprintf("length %d, want %d", l, n), msgAndArgs)
}

// NotEmpty checks that v has a non-zero length.
func NotEmpty(t testing.TB, v any, msgAndArgs ...any) bool {
	t.Helper()
	if testutil.Len(v) > 0 {
		return true
	}
	return fail(t, "expected non-empty value", msgAndArgs)
}

// NotNil checks that v is not nil.
func NotNil(t testing.TB, v any, msgAndArgs ...any) bool {
	t.Helper()
	if !testutil.IsNil(v) {
		return true
	}
	return fail(t, "expected non-nil value", msgAndArgs)
}

// Greater checks a > b.
func Greater[T cmp.Ordered](t testing.TB, a, b T, msgAndArgs ...any) bool {
	t.Helper()
	if a > b {
		return true
	}
	return fail(t, sprintf("%v is not greater than %v", a, b), msgAndArgs)
}

// Less checks a < b.
func Less[T cmp.Ordered](t testing.TB, a, b T, msgAndArgs ...any) bool {
	t.Helper()
	if a < b {
		return true
	}
	return fail(t, sprintf("%v is not less than %v", a, b), msgAndArgs)
}

// LessOrEqual checks a <= b.
func LessOrEqual[T cmp.Ordered](t testing.TB, a, b T, msgAndArgs ...any) bool {
	t.Helper()
	if a <= b {
		return true
	}
	return fail(t, sprintf("%v is greater than %v", a, b), msgAndArgs)
}

// InDelta checks |a-b| <= delta.
func InDelta(t testing.TB, a, b, delta float64, msgAndArgs ...any) bool {
	t.Helper()
	if math.Abs(a-b) <= delta {
		return true
	}
	return fail(t, sprintf("%v and %v differ by more than %v", a, b, delta), msgAndArgs)
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
