package golang

import (
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestLoadDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want string
	}{
		{"no module", "no required module provides package github.com/spf13/cobra; to add it:", "dependencies_unavailable"},
		{"offline", "github.com/x/y@v1.0.0: module lookup disabled by GOPROXY=off", "dependencies_unavailable"},
		{"go.sum", "missing go.sum entry for module providing package example.com/a", "dependencies_unavailable"},
		{"import", `could not import github.com/a/b (no required module provides package "github.com/a/b")`, "dependencies_unavailable"},
		{"syntax", "x.go:3:1: expected declaration, found foo", "package_load_failed"},
		{"type", "undefined: foo", "package_load_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &packages.Package{Errors: []packages.Error{{Msg: tc.msg}}}
			if got := loadDiagnostic(p); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
