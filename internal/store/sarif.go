package store

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/sarifimport"
)

// SARIFStore is implemented by stores that keep findings of external
// security tools imported from SARIF (internal/sarifimport). Like runtime
// facts they live apart from the code graph, per namespace (the graph name,
// or "org|graph"), so a re-index keeps them and organisations sharing a
// graph never see each other's uploads; deleting the graph removes them.
type SARIFStore interface {
	// SARIFFindings returns ErrNotFound when the namespace has none.
	SARIFFindings(ctx context.Context, ns string) (sarifimport.Set, error)
	SetSARIFFindings(ctx context.Context, ns string, set sarifimport.Set) error
	DeleteSARIFFindings(ctx context.Context, ns string) error
}
