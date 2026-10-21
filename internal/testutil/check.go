// Package testutil holds the comparison logic behind the assert and require
// test helpers. It is small on purpose: the project's tests only need
// equality, containment, length and ordering checks.
package testutil

import (
	"cmp"
	"fmt"
	"reflect"
	"strings"
)

// Equal reports whether got and want are deeply equal.
func Equal(got, want any) bool { return reflect.DeepEqual(got, want) }

// Contains reports whether haystack (string, slice, array or map keys)
// contains needle.
func Contains(haystack, needle any) bool {
	if s, ok := haystack.(string); ok {
		n, ok := needle.(string)
		return ok && strings.Contains(s, n)
	}
	v := reflect.ValueOf(haystack)
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if reflect.DeepEqual(v.Index(i).Interface(), needle) {
				return true
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if reflect.DeepEqual(k.Interface(), needle) {
				return true
			}
		}
	}
	return false
}

// Len returns the length of v, or -1 when v has no length.
func Len(v any) int {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.String, reflect.Chan:
		return rv.Len()
	}
	return -1
}

// IsNil reports whether v is nil or a nil pointer, map, slice, func, chan or interface.
func IsNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// Compare orders a and b.
func Compare[T cmp.Ordered](a, b T) int { return cmp.Compare(a, b) }

// Message formats optional trailing message arguments: either a single
// value or a format string followed by arguments.
func Message(msgAndArgs []any) string {
	switch len(msgAndArgs) {
	case 0:
		return ""
	case 1:
		return fmt.Sprint(msgAndArgs[0])
	}
	if f, ok := msgAndArgs[0].(string); ok {
		return fmt.Sprintf(f, msgAndArgs[1:]...)
	}
	return fmt.Sprint(msgAndArgs...)
}
