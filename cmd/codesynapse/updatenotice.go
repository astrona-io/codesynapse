package main

import (
	"encoding/json"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/cliversion"
)

// updateNotice is the one notice a session starts with when this CLI is
// behind the server (serverVersion, from the rules the hook fetches anyway)
// or the Claude Code plugin at pluginRoot does not run the hooks this CLI
// ships. It is "" when everything matches, when nothing can be compared, and
// always for a development build. It reads one local file and never the
// network.
func updateNotice(cli, serverVersion, pluginRoot string) string {
	if !cliversion.IsRelease(cli) {
		return ""
	}
	if cliversion.Behind(cli, serverVersion) {
		return cliversion.UpgradeNotice(cli, serverVersion)
	}
	if pluginRoot == "" {
		return ""
	}
	installed, err := agentcfg.ReadPluginHooks(pluginRoot)
	if err != nil {
		return ""
	}
	switch missing, extra := agentcfg.CompareHooks(installed); {
	case len(extra) > 0:
		return cliversion.PluginAheadNotice(cli)
	case len(missing) > 0:
		return cliversion.PluginBehindNotice(cli, len(missing))
	}
	return ""
}

// withUpgradeNotice adds the upgrade notice to the server instructions of
// an initialize answer when this bridge is an older release than the MCP
// server (its serverInfo version), so agents without the plugin's session
// hook (Codex, Cursor) tell the person too. Any other answer, or one that
// does not parse, is returned as it came.
func withUpgradeNotice(data []byte, cli string) []byte {
	var msg map[string]json.RawMessage
	if json.Unmarshal(data, &msg) != nil || msg["result"] == nil {
		return data
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(msg["result"], &result) != nil {
		return data
	}
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(result["serverInfo"], &info) != nil || !cliversion.Behind(cli, info.Version) {
		return data
	}
	var instructions string
	if raw, ok := result["instructions"]; ok && json.Unmarshal(raw, &instructions) != nil {
		return data
	}
	if instructions != "" {
		instructions += "\n\n"
	}
	var err error
	if result["instructions"], err = json.Marshal(instructions + cliversion.ForAgent(cliversion.UpgradeNotice(cli, info.Version))); err != nil {
		return data
	}
	if msg["result"], err = json.Marshal(result); err != nil {
		return data
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return data
	}
	return out
}
