package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// findingsOf indexes one Dockerfile and returns "rule:line" for each finding.
func findingsOf(t *testing.T, name, content string) []string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	g, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	var out []string
	for _, n := range g.Nodes() {
		// Other rules read Dockerfiles too (dependency-updates-missing).
		if n.Kind == model.KindFinding && strings.HasPrefix(n.Rule, "dockerfile-") {
			assert.Equal(t, n.Language, "dockerfile")
			out = append(out, fmt.Sprintf("%s:%02d", n.Rule, n.StartLine))
		}
	}
	slices.Sort(out)
	return out
}

func TestDockerfileRules(t *testing.T) {
	for _, tt := range []struct {
		name, file, content string
		want                []string
	}{
		{"everything wrong", "Dockerfile", `FROM ubuntu
ADD https://example.com/tool.tar.gz /opt/
RUN apt-get update \
 && curl -fsSL https://example.com/install.sh | sudo bash
ENV DB_PASSWORD=Sup3rS3cretValue
ARG API_TOKEN="not-a-real-token"
CMD ["app"]
`, []string{"dockerfile-add-url:02", "dockerfile-curl-pipe-shell:03", "dockerfile-image-unpinned:01", "dockerfile-root-user:01", "dockerfile-secret-in-env:05", "dockerfile-secret-in-env:06"}},
		{"done right", "Containerfile", `# syntax=docker/dockerfile:1
FROM golang:1.23 AS build
RUN go build -o /app .
FROM gcr.io/distroless/static:nonroot
ADD --checksum=sha256:24454f830cdb571e2c4ad15481119c43b3cafd48dd869a9b2945d1036d1dc68d https://example.com/x.tgz /x
COPY --from=build /app /app
ENV PASSWORD_FILE=/run/secrets/db TOKEN_URL=https://auth.example.com
ARG GITHUB_TOKEN
ENTRYPOINT ["/app"]
`, nil},
		{"user set in an earlier stage", "Dockerfile.prod", `FROM alpine:3.20 AS base
RUN adduser -D app
USER app
FROM base AS final
CMD ["app"]
`, nil},
		{"root again in the final stage", "api.Dockerfile", `FROM alpine:3.20
USER app
FROM alpine:3.24
USER root
CMD ["app"]
`, []string{"dockerfile-root-user:03"}},
		{"heredoc and continuation do not hide lines", "Dockerfile", "FROM alpine:3.20\nRUN <<EOF\necho hi\nUSER root\nEOF\nUSER 10001\nRUN echo a \\\n # a comment\n && curl https://x.sh | sh\n", []string{"dockerfile-curl-pipe-shell:07"}},
		{"escape directive", "Dockerfile", "# escape=`\nFROM mcr.microsoft.com/windows/nanoserver:ltsc2022\nUSER ContainerUser\nRUN echo a `\n  && echo b\n", nil},
		{"scratch and ARG images", "Dockerfile", "ARG BASE=alpine:3.20\nFROM ${BASE}\nUSER 1000:1000\nFROM scratch\nCOPY --from=0 /x /x\n", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, findingsOf(t, tt.file, tt.content), tt.want)
		})
	}
}

func TestDockerfileNode(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM golang:1.23 AS build\nFROM alpine:3.20\nUSER 1\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.dockerfile.md"), []byte("FROM ubuntu\n"), 0o600))
	g, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	n, ok := g.Node(model.NodeID("r", model.KindConfig, "Dockerfile"))
	require.True(t, ok)
	assert.Equal(t, n.Language, "dockerfile")
	assert.Equal(t, n.Summary, "Dockerfile: 2 stage(s), from golang:1.23, alpine:3.20")
	_, ok = g.Node(model.NodeID("r", model.KindConfig, "notes.dockerfile.md"))
	assert.False(t, ok, "not a Dockerfile")
}

func FuzzParseDockerfile(f *testing.F) {
	f.Add([]byte("FROM a\nRUN <<EOF\nx\nEOF\nRUN a \\\n# c\n b\n"))
	f.Add([]byte("# escape=`\nRUN a `\n"))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_ = parseDockerfile(data)
	})
}
