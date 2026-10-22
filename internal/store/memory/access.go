package memory

import (
	"context"
	"fmt"
	"slices"

	"github.com/astrona-io/codesynapse/internal/model"
)

// ReplaceAccess installs a validated, complete revision.
func (s *Store) ReplaceAccess(_ context.Context, snapshot model.AccessSnapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.access == nil {
		s.access = map[string]model.AccessSnapshot{}
	}
	if s.access[snapshot.TenantID].Revision > snapshot.Revision {
		return nil
	}
	if previous := s.access[snapshot.TenantID]; previous.Revision == snapshot.Revision {
		if previous.Digest() != snapshot.Digest() {
			return fmt.Errorf("access revision already has a different policy")
		}
		return nil
	}
	snapshot.Users = slices.Clone(snapshot.Users)
	snapshot.Teams = slices.Clone(snapshot.Teams)
	snapshot.Repos = slices.Clone(snapshot.Repos)
	snapshot.Members = slices.Clone(snapshot.Members)
	snapshot.Grants = slices.Clone(snapshot.Grants)
	s.access[snapshot.TenantID] = snapshot
	return nil
}

// AccessAt implements store.AccessStore.
func (s *Store) AccessAt(_ context.Context, tenant string, revision int64, userID, repoID string) (model.AccessDecision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	x, ok := s.access[tenant]
	if !ok || revision < 1 || x.Revision != revision {
		return model.AccessDecision{}, nil
	}
	return x.Evaluate(userID, repoID), nil
}
