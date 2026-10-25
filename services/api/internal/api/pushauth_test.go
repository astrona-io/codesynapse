package api

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestInstallPushAuth(t *testing.T) {
	const remote = "https://github.com/acme/app.git"
	read := gitx.Auth{Host: "github.com", Token: "read-token"}
	push := gitx.Auth{Host: "github.com", Token: "push-token"}
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"push tokens win", Config{GitAuth: read, PushAuths: []gitx.Auth{push}}, "push-token"},
		{"fall back to the read token", Config{GitAuth: read}, "read-token"},
		{"push tokens for another host do not leak", Config{PushAuths: []gitx.Auth{{Host: "codeberg.org", Token: "x"}}, GitAuth: read}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{cfg: tt.cfg}
			assert.Equal(t, s.installPushAuth(remote).Token, tt.want)
		})
	}
}
