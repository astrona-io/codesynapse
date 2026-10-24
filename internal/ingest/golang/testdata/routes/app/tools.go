package app

import (
	"github.com/spf13/cobra"

	"example.com/routes/mcp"
)

func Tools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "search"}, search)
	mcp.AddTool(srv, tool("lookup", "Finds one thing."), lookup)
}

func tool(name, _ string) *mcp.Tool { return &mcp.Tool{Name: name} }

func search() error { return nil }

func lookup() error { return nil }

var serveCmd = &cobra.Command{Use: "serve [addr]", RunE: serve}

// A command that only groups others runs nothing.
var adminCmd = &cobra.Command{Use: "admin"}

func serve(*cobra.Command, []string) error { return nil }
