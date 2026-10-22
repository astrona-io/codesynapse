// Package execution carries the runtime's immutable chat identity. It does not
// create session identifiers or replace authentication and session resolution.
package execution

import (
	"context"
	"errors"
)

// Runtime metadata is supplied outside model-controlled tool arguments.
const (
	SessionHeader       = "X-CodeSynapse-Session"
	ParentSessionHeader = "X-CodeSynapse-Parent-Session"
)

// Identity is the non-secret envelope inherited by delegated work.
type Identity struct {
	TenantID        string `json:"tenant_id"`
	SessionID       string `json:"session_id"`
	SubjectID       string `json:"subject_id"`
	ParentSessionID string `json:"parent_session_id"`
}

// Errors deliberately exclude rejected identity values.
var (
	ErrMissing  = errors.New("runtime session context is required; register the runtime-provided session before executing tools")
	ErrInvalid  = errors.New("invalid runtime session context")
	ErrConflict = errors.New("runtime session context cannot change during an execution")
)

// Validate checks the serialized envelope before it can enter a context.
func Validate(id Identity) error {
	if !validID(id.TenantID) || !validID(id.SubjectID) || !validID(id.SessionID) || id.ParentSessionID != id.SessionID {
		return ErrInvalid
	}
	return nil
}

func validID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-' {
			continue
		}
		return false
	}
	return true
}

type identityKey struct{}

// With binds an already-authorized identity once. Rebinding any field fails.
func With(ctx context.Context, id Identity) (context.Context, error) {
	if ctx == nil {
		return nil, ErrMissing
	}
	if err := Validate(id); err != nil {
		return nil, err
	}
	if previous, ok := From(ctx); ok {
		if previous != id {
			return nil, ErrConflict
		}
		return ctx, nil
	}
	return context.WithValue(ctx, identityKey{}, id), nil
}

// From returns a copy; callers cannot mutate an identity already in context.
func From(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Require rejects work without a live, validated inherited context.
func Require(ctx context.Context) (Identity, error) {
	id, ok := From(ctx)
	if !ok {
		return Identity{}, ErrMissing
	}
	if err := Validate(id); err != nil {
		return Identity{}, err
	}
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	return id, nil
}
