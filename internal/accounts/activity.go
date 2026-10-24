package accounts

import (
	"context"
	"encoding/json"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/execution"
)

// RecordHookActivity uses the existing durable, tenant-owned audit stream. Its
// runtime context envelope permits the same session filters as MCP requests;
// agent.hook actions are observations, never counted as MCP invocations.
func (s *Service) RecordHookActivity(ctx context.Context, p Principal, id execution.Identity, activity agentcfg.HookActivity) (Event, error) {
	if execution.Validate(id) != nil {
		return Event{}, ErrInvalidInput
	}
	if err := activity.Validate(); err != nil {
		return Event{}, ErrInvalidInput
	}
	if id.SubjectID != p.User.ID || id.TenantID == "" || (p.Via == "key" && p.OrgID != id.TenantID) {
		return Event{}, ErrForbidden
	}
	return policyChange(ctx, s, func(tx *Service) (Event, error) {
		if err := executionMember(ctx, tx.St, id.SubjectID, id.TenantID); err != nil {
			return Event{}, err
		}
		record, err := tx.St.ExecutionSessionByID(ctx, id.SessionID)
		if err != nil || record.UserID != id.SubjectID || record.OrgID != id.TenantID || !record.Active(tx.now()) {
			return Event{}, ErrExecutionSessionClosed
		}
		detail, err := json.Marshal(activity.HookMeasurements)
		if err != nil {
			return Event{}, err
		}
		e := actorEvent(p, "agent.hook."+activity.Client+"."+activity.Event, id.TenantID, "")
		e.Detail = string(detail)
		e.MCP = &MCPRequest{Method: "agent/hook", Client: activity.Client, SessionID: id.SessionID, Name: activity.Event}
		if activity.Outcome == "error" {
			e.Outcome = Failure
		}
		return tx.Record(ctx, e)
	})
}
