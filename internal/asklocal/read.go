package asklocal

import (
	"fmt"
	"strings"
)

// read_ranges: the agent has file:start-end ranges from minimal_context,
// file_outline or an ask_local citation and wants the exact lines, often of
// several files. One call returns them all with line numbers, read from the
// checkout by the bridge under the same rules as ask_local (files git
// tracks only, no symlinks or binaries). Nothing is sent to a model or the
// server; the text goes to the agent that asked, as its own Read would.

// ReadTool is the MCP tool name.
const ReadTool = "read_ranges"

// MaxReadBytes caps the text of one call: ranges are for reading narrowly,
// and a call that needs more should be split or asked of ask_local.
const MaxReadBytes = 256 << 10

// ReadIn is the tool's input.
type ReadIn struct {
	Ranges []string `json:"ranges" jsonschema:"repository-relative file:start-end ranges, e.g. internal/api/api.go:100-140 (only files git tracks are read)"`
}

// Validate checks that every entry is a file:start-end range: whole files
// and directories are what the ranges are there to avoid.
func (in ReadIn) Validate() error {
	switch {
	case len(in.Ranges) == 0:
		return fmt.Errorf("%w: ranges is empty", ErrInvalid)
	case len(in.Ranges) > MaxPaths:
		return fmt.Errorf("%w: more than %d ranges", ErrInvalid, MaxPaths)
	}
	for _, r := range in.Ranges {
		_, start, _, err := ParseSpec(r)
		if err != nil {
			return err
		}
		if start == 0 {
			return fmt.Errorf("%w: %q has no line range; give file:start-end (file_outline lists the ranges)", ErrInvalid, r)
		}
	}
	return nil
}

// RenderRanges writes the files as numbered blocks, in the order read, and
// names what was not read at the end.
func RenderRanges(files []File, skipped []string) string {
	var b strings.Builder
	for i, f := range files {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(Numbered(f))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "\nNot read (binary, too large or a symlink): %s\n", strings.Join(skipped, ", "))
	}
	return b.String()
}
