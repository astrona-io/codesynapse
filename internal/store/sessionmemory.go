package store

import (
	"context"
	"time"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
)

// SessionMemoryStore contains ephemeral chat facts and never repository memory.
type SessionMemoryStore interface {
	UpsertSessionMemory(context.Context, sessionmemory.Scope, []memory.Triple, string, time.Time) error
	ReadSessionMemory(context.Context, sessionmemory.Scope, string, int) (memory.Graph, error)
	DeleteSessionMemory(context.Context, sessionmemory.Scope) error
	DeleteExecutionMemory(context.Context, string, string) error
	SetSessionMemoryEmbeddings(context.Context, sessionmemory.Scope, string, map[string][]float32) error
	SessionMemoryNearest(context.Context, sessionmemory.Scope, string, []float32, int) ([]memory.Scored, error)
}
