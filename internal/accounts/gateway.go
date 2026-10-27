package accounts

import (
	"context"
	"fmt"
)

// The agent gateway sends an organisation's agent model traffic through
// CodeSynapse. It is off until an owner or admin of the organisation turns it
// on from a signed-in session; the operator's switch alone is not enough.

// GatewayEnabled says whether the organisation opted in. It checks no
// caller: the gateway asks it for the organisation of a key it authenticated.
func (s *Service) GatewayEnabled(ctx context.Context, orgID string) (bool, error) {
	return s.St.GatewayEnabled(ctx, orgID)
}

// GatewaySetting returns the opt-in for a member of the organisation.
func (s *Service) GatewaySetting(ctx context.Context, p Principal, orgID string) (bool, error) {
	if err := s.Require(ctx, p, orgID, Member); err != nil {
		return false, err
	}
	return s.St.GatewayEnabled(ctx, orgID)
}

// SetGateway turns the gateway on or off for the organisation. Only owners
// and admins in a signed-in session may; the change is audited first, and a
// failed audit refuses it.
func (s *Service) SetGateway(ctx context.Context, p Principal, orgID string, enabled bool) error {
	if err := signedIn(p); err != nil {
		return err
	}
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return err
	}
	e := actorEvent(p, "org.gateway.set", orgID, orgID)
	e.Detail = fmt.Sprintf("enabled=%t", enabled)
	if _, err := s.Record(ctx, e); err != nil {
		return err
	}
	return s.St.SetGatewayEnabled(ctx, orgID, enabled)
}
