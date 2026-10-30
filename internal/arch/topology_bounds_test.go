package arch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/ingest/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// topoOf builds the topology of a repository holding only files: no Go
// packages, so only images, services and workloads take part.
func topoOf(t *testing.T, files map[string]string) *model.Graph {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	g := model.NewGraph()
	cfg, err := config.Extract(context.Background(), topoRepo, root)
	require.NoError(t, err)
	g.Merge(cfg)
	require.NoError(t, BuildTopology(context.Background(), topoRepo, root, g))
	return g
}

func components(g *model.Graph) []model.Node {
	var out []model.Node
	for _, n := range g.Nodes() {
		if n.Kind == model.KindComponent {
			out = append(out, n)
		}
	}
	return out
}

func TestTopologyNamesStayUnique(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  map[string]string // component -> file:line of its stage
	}{
		{
			name:  "suffix taken by a stage",
			files: map[string]string{"Dockerfile": "FROM alpine AS api-2\nFROM alpine AS api\nFROM alpine AS api\n"},
			want:  map[string]string{"api-2": "Dockerfile:1", "api": "Dockerfile:2", "api-3": "Dockerfile:3"},
		},
		{
			name: "suffix taken by a compose service",
			files: map[string]string{
				"a/Dockerfile":       "FROM alpine\n",
				"b/Dockerfile":       "FROM alpine AS api\n",
				"c/Dockerfile":       "FROM alpine AS api\n",
				"docker-compose.yml": "services:\n  api-2:\n    build: a\n",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := topoOf(t, tt.files)
			got := map[string]string{}
			for _, n := range components(g) {
				_, dup := got[n.Name]
				assert.False(t, dup, "component %s twice", n.Name)
				got[n.Name] = fmt.Sprintf("%s:%d", n.File, n.StartLine)
				top, ok := TopologyOf(n)
				require.True(t, ok)
				builds := 0
				for _, m := range top.Members {
					if m.Role == RoleImageBuild {
						builds++
					}
				}
				assert.Equal(t, builds, 1, "image builds of %s", n.Name)
			}
			if tt.want != nil {
				assert.Equal(t, fmt.Sprint(got), fmt.Sprint(tt.want))
				return
			}
			assert.Len(t, got, 3)
			assert.Equal(t, got["api-2"], "a/Dockerfile:1")
		})
	}
}

func TestStageBuildsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("FROM golang AS big\nRUN go build " + strings.Repeat("./a ", 250_000))
	for i := range 1000 {
		fmt.Fprintf(&b, "./d%d ", i)
	}
	b.WriteString("\n")
	prev := "big"
	for i := range 300 {
		fmt.Fprintf(&b, "FROM %s AS t%d\n", prev, i)
		for j := range 30 {
			fmt.Fprintf(&b, "COPY --from=%s / /\n", cmpStage(i-j-1))
		}
		prev = fmt.Sprintf("t%d", i)
	}
	require.True(t, b.Len() > 1<<20, "the file is over 1 MiB")

	start := time.Now()
	df := parseStages("Dockerfile", []byte(b.String()))
	assert.True(t, len(df.stages) <= maxStages, "stages %d", len(df.stages))
	assert.True(t, len(df.stages[0].builds) <= maxStageBuilds, "builds %d", len(df.stages[0].builds))
	links := 0
	for i, s := range df.stages {
		assert.True(t, len(s.copies) <= maxStageCopies, "copies of stage %d: %d", i, len(s.copies))
		got := df.stageBuilds(i)
		assert.True(t, len(got) <= maxStageLinks, "links of stage %d: %d", i, len(got))
		links += len(got)
	}
	assert.True(t, links <= maxStages*maxStageLinks, "links %d", links)
	assert.True(t, time.Since(start) < 2*time.Second, "took %s", time.Since(start))
}

func cmpStage(i int) string {
	if i < 0 {
		return "big"
	}
	return fmt.Sprintf("t%d", i)
}

func TestStageBuildsKeepsEveryCopy(t *testing.T) {
	df := parseStages("Dockerfile", []byte(`FROM golang AS build
RUN go build -o /out/api ./cmd/api
RUN go build -o /out/worker ./cmd/worker
FROM alpine
COPY --from=build /out/api /api
COPY --from=build /out/worker /worker
`))
	var got []string
	for _, m := range df.stageBuilds(len(df.stages) - 1) {
		got = append(got, fmt.Sprintf("%s candidate=%t", m.build.dir, m.candidate))
	}
	slices.Sort(got)
	assert.Equal(t, strings.Join(got, ", "), "cmd/api candidate=false, cmd/worker candidate=false")
}

func TestTopologyCapsText(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 36)
	long := strings.Repeat("x", 5000)
	g := topoOf(t, map[string]string{
		"Dockerfile": "FROM alpine AS " + long + "\nFROM alpine AS " + token + "\n",
		"docker-compose.yml": "services:\n  " + long + "-" + token + ":\n    build: .\n    environment:\n      " +
			strings.Repeat("K", 5000) + ": v\n",
	})
	got := components(g)
	assert.Equal(t, len(got), 2)
	for _, n := range got {
		all := n.ID + n.Name + n.QualifiedName + n.Meta + n.Summary
		assert.NotContains(t, all, token, n.Name)
		assert.True(t, len(n.Name) <= maxSlug, "name of %d bytes", len(n.Name))
		assert.True(t, len(n.Meta) < 8<<10, "meta of %d bytes", len(n.Meta))
		top, ok := TopologyOf(n)
		require.True(t, ok)
		for _, m := range top.Members {
			assert.True(t, len([]rune(m.Name)) <= maxNameLen, "member name of %d runes", len([]rune(m.Name)))
			assert.True(t, len([]rune(m.Evidence)) <= maxEvidenceLen, "evidence of %d runes", len([]rune(m.Evidence)))
		}
		for _, k := range top.Env {
			assert.True(t, len([]rune(k.Name)) <= maxNameLen, "env key of %d runes", len([]rune(k.Name)))
		}
	}
}
