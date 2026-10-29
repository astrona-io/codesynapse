package accounts

import (
	"context"
	"fmt"
)

// The agent gateway sends an organisation's agent model traffic through
// CodeSynapse, which cuts cost and tokens and keeps what a local model can
// answer on the premises. It is on unless an owner or admin of the
// organisation turns it off from a signed-in session.

// GatewayEnabled says whether the gateway is on for the organisation (it is
// unless someone switched it off). It checks no caller: the gateway asks it
// for the organisation of a key it authenticated.
func (s *Service) GatewayEnabled(ctx context.Context, orgID string) (bool, error) {
	return s.St.GatewayEnabled(ctx, orgID)
}

// GatewayLocalFirst says whether the organisation's gateway offers every
// model's requests to the local model first (it does unless someone switched
// it off). Like GatewayEnabled it checks no caller.
func (s *Service) GatewayLocalFirst(ctx context.Context, orgID string) (bool, error) {
	return s.St.GatewayLocalFirst(ctx, orgID)
}

// GatewaySettings is an organisation's gateway choice.
type GatewaySettings struct {
	Enabled        bool
	LocalFirst     bool
	ShortenPrompts bool
}

// GatewaySetting returns the settings for a member of the organisation.
func (s *Service) GatewaySetting(ctx context.Context, p Principal, orgID string) (GatewaySettings, error) {
	if err := s.Require(ctx, p, orgID, Member); err != nil {
		return GatewaySettings{}, err
	}
	on, err := s.St.GatewayEnabled(ctx, orgID)
	if err != nil {
		return GatewaySettings{}, err
	}
	first, err := s.St.GatewayLocalFirst(ctx, orgID)
	if err != nil {
		return GatewaySettings{}, err
	}
	shorten, err := s.St.GatewayShortenPrompts(ctx, orgID)
	return GatewaySettings{Enabled: on, LocalFirst: first, ShortenPrompts: shorten}, err
}

// GatewayShortenPrompts says whether the organisation's gateway has the local
// model shorten prose people type before a request goes to the cloud (off
// unless someone turned it on). Like GatewayEnabled it checks no caller.
func (s *Service) GatewayShortenPrompts(ctx context.Context, orgID string) (bool, error) {
	return s.St.GatewayShortenPrompts(ctx, orgID)
}

// SetGatewayShortenPrompts turns prompt shortening on the cloud leg on or
// off. It alters the words people typed, so it is off by default. Same rules
// as SetGateway.
func (s *Service) SetGatewayShortenPrompts(ctx context.Context, p Principal, orgID string, on bool) error {
	if err := signedIn(p); err != nil {
		return err
	}
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return err
	}
	e := actorEvent(p, "org.gateway.shorten_prompts.set", orgID, orgID)
	e.Detail = fmt.Sprintf("shorten_prompts=%t", on)
	if _, err := s.Record(ctx, e); err != nil {
		return err
	}
	return s.St.SetGatewayShortenPrompts(ctx, orgID, on)
}

// SetGatewayLocalFirst chooses whether every model's requests go to the
// local model first or only the small model's, for organisations whose
// local model is too small for the main one. Same rules as SetGateway.
func (s *Service) SetGatewayLocalFirst(ctx context.Context, p Principal, orgID string, on bool) error {
	if err := signedIn(p); err != nil {
		return err
	}
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return err
	}
	e := actorEvent(p, "org.gateway.local_first.set", orgID, orgID)
	e.Detail = fmt.Sprintf("local_first=%t", on)
	if _, err := s.Record(ctx, e); err != nil {
		return err
	}
	return s.St.SetGatewayLocalFirst(ctx, orgID, on)
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
