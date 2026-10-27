-- The agent gateway is on by default: an organisation without a row counts as
-- enabled (see GatewayEnabled), and a row written without a value follows suit.
-- Choices already stored are kept.
ALTER TABLE org_gateway ALTER COLUMN enabled SET DEFAULT true;
