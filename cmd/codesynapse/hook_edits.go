package main

import (
	"os"
	"path/filepath"
)

func noteEdit(in toolHookInput, failed bool) {
	state := stateFile(in.SessionID)
	if state == "" {
		return
	}
	s := loadState(state)
	e := &s.Edits
	switch in.ToolName {
	case "Edit", "apply_patch":
		e.Edits++
		payload := len(in.ToolInput.OldString) + len(in.ToolInput.NewString) + len(in.ToolInput.Patch)
		e.PatchBytes += payload
		// A patch without a single known file has no defensible full-rewrite
		// baseline. Failed edits still cost output but earn no savings.
		if !failed && payload > 0 && in.ToolInput.FilePath != "" {
			path := in.ToolInput.FilePath
			if !filepath.IsAbs(path) {
				path = filepath.Join(in.Cwd, path)
			}
			if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.Size() <= 16<<20 {
				e.BaselineBytes += int(fi.Size())
				e.AvoidedBytes += max(0, int(fi.Size())-payload)
			}
		}
	case "Write":
		e.Writes++
		e.WriteBytes += len(in.ToolInput.Content)
	default:
		return
	}
	if failed {
		e.Failed++
	} else {
		noteEditDir(&s, in)
	}
	if e.Validate() == nil {
		saveState(state, s)
	}
}
