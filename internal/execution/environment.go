package execution

import (
	"context"
	"strings"
)

// Environment returns a separate subprocess environment. Runtime fields from
// the parent process are discarded: only validated request context may supply
// them. Offline maintenance without execution context still runs without them.
func Environment(ctx context.Context, base []string) ([]string, error) {
	env := make([]string, 0, len(base)+4)
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "CODESYNAPSE_TENANT_ID", "CODESYNAPSE_SESSION_ID", "CODESYNAPSE_PARENT_SESSION_ID", "CODESYNAPSE_SUBJECT_ID":
		default:
			env = append(env, entry)
		}
	}
	id, ok := From(ctx)
	if !ok {
		return env, nil
	}
	if err := Validate(id); err != nil {
		return nil, err
	}
	return append(env, "CODESYNAPSE_TENANT_ID="+id.TenantID, "CODESYNAPSE_SESSION_ID="+id.SessionID,
		"CODESYNAPSE_PARENT_SESSION_ID="+id.ParentSessionID, "CODESYNAPSE_SUBJECT_ID="+id.SubjectID), nil
}
