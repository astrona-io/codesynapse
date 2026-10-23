-- MCP request details on audit entries (docs/SAAS.md, 5c). Null for every
-- other entry, which keeps their hashes unchanged.
ALTER TABLE events ADD COLUMN mcp jsonb;
CREATE INDEX events_mcp_session ON events ((mcp->>'session_id')) WHERE mcp IS NOT NULL;
