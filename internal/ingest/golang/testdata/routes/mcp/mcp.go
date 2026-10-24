// Package mcp stands in for an MCP server library.
package mcp

// Server serves tools.
type Server struct{}

// Tool describes a tool.
type Tool struct{ Name string }

// AddTool registers a tool.
func AddTool(*Server, *Tool, func() error) {}
