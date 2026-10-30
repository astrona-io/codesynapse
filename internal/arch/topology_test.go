package arch

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/ingest/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const topoRepo = "shop"

// topoFixture is a small repository with two images built from one
// Dockerfile, the compose file and manifests that run them, a chart and a
// second Dockerfile whose image would get the same name.
var topoFixture = map[string]string{
	"cmd/api/main.go": `package main

import (
	"os"

	"example.com/shop/internal/conf"
)

func main() { _ = os.Getenv("PORT"); conf.Load() }
`,
	"cmd/worker/main.go": `package main

import sys "os"

func main() { _ = sys.Getenv("QUEUE") }
`,
	"internal/conf/conf.go": `package conf

import "os"

const keyDB = "DATABASE_URL"

func Load() { _, _ = os.LookupEnv(keyDB); _ = os.Getenv("lower-case is no env name") }
`,
	"Dockerfile": `FROM golang:1.27 AS build
WORKDIR /src
COPY . .
RUN go build -o /out/api ./cmd/api && \
    go build -o /out/worker ./cmd/worker

FROM alpine AS api
COPY --from=build /out/api /usr/local/bin/api

FROM alpine AS worker
COPY --from=build /out/ /usr/local/bin/
`,
	"docker-compose.yml": `x-env: &common
  LOG_LEVEL: info
services:
  api:
    build:
      context: .
      dockerfile: Dockerfile
      target: api
    image: ghcr.io/acme/shop-api:1.0
    environment:
      <<: *common
      PORT: "8080"
      DATABASE_URL: postgres://user:hunter2@db/shop
  worker:
    build:
      context: .
      target: worker
    environment:
      - QUEUE=jobs
`,
	"deploy/api.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: shop-api
spec:
  template:
    spec:
      containers:
        - name: api
          image: ghcr.io/acme/shop-api:2.0
          env:
            - name: PORT
              value: "9090"
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef: {name: db, key: url}
`,
	"deploy/worker.yaml": `apiVersion: batch/v1
kind: CronJob
metadata:
  name: nightly
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: run
              image: registry.example.com/other/worker:3
`,
	"chart/Chart.yaml": "apiVersion: v2\nname: shop\nversion: 0.1.0\n",
	"chart/values.yaml": `image:
  repository: ghcr.io/acme/shop-api
  tag: "1.0"
`,
	"tools/api/Dockerfile": "FROM alpine\nRUN echo hi\n",
}

func writeTopoFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range topoFixture {
		p := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	return root
}

// topoGraph builds the nodes the code extractor would: packages, files and
// in-repo imports, then merges the configuration graph.
func topoGraph(t *testing.T, root string) *model.Graph {
	t.Helper()
	g := model.NewGraph()
	pkgs := map[string]string{"cmd/api": "main", "cmd/worker": "main", "internal/conf": "conf"}
	for dir, name := range pkgs {
		p := "example.com/shop/" + dir
		g.AddNode(model.Node{ID: model.NodeID(topoRepo, model.KindPackage, p), Kind: model.KindPackage, Repo: topoRepo, Name: name, QualifiedName: p})
	}
	for rel := range topoFixture {
		if path.Ext(rel) != ".go" {
			continue
		}
		g.AddNode(model.Node{ID: model.NodeID(topoRepo, model.KindFile, rel), Kind: model.KindFile, Repo: topoRepo, File: rel,
			Package: "example.com/shop/" + path.Dir(rel)})
	}
	g.AddEdge(model.Edge{From: model.NodeID(topoRepo, model.KindPackage, "example.com/shop/cmd/api"),
		To: model.NodeID(topoRepo, model.KindPackage, "example.com/shop/internal/conf"), Kind: model.EdgeImports})
	cfg, err := config.Extract(context.Background(), topoRepo, root)
	require.NoError(t, err)
	g.Merge(cfg)
	return g
}

func component(t *testing.T, g *model.Graph, name string) (model.Node, Topology) {
	t.Helper()
	n, ok := g.Node(model.NodeID(topoRepo, model.KindComponent, name))
	require.True(t, ok, "component %s", name)
	top, ok := TopologyOf(*n)
	require.True(t, ok, "topology of %s", name)
	return *n, top
}

func member(top Topology, role, name string) (Member, bool) {
	i := slices.IndexFunc(top.Members, func(m Member) bool { return m.Role == role && m.Name == name })
	if i < 0 {
		return Member{}, false
	}
	return top.Members[i], true
}

func envKey(top Topology, name string) (EnvKey, bool) {
	i := slices.IndexFunc(top.Env, func(k EnvKey) bool { return k.Name == name })
	if i < 0 {
		return EnvKey{}, false
	}
	return top.Env[i], true
}

func TestTopology(t *testing.T) {
	root := writeTopoFixture(t)
	g := topoGraph(t, root)
	require.NoError(t, BuildTopology(context.Background(), topoRepo, root, g))

	t.Run("members and evidence", func(t *testing.T) {
		_, api := component(t, g, "api")
		tests := []struct {
			role, name string
			candidate  bool
		}{
			{RoleImageBuild, "api", false},
			{RoleGoMain, "example.com/shop/cmd/api", false},
			{RoleCompose, "api", false},
			{RoleWorkload, "Deployment/shop-api container api", false},
			{RoleHelmValues, "image", true},
		}
		for _, tt := range tests {
			m, ok := member(api, tt.role, tt.name)
			if assert.True(t, ok, "%s %s", tt.role, tt.name) {
				assert.Equal(t, m.Candidate, tt.candidate, "%s %s", tt.role, tt.name)
				assert.NotEqual(t, m.Evidence, "", "%s %s", tt.role, tt.name)
			}
		}
		// The api stage copies /out/api only, so the worker binary is not in it.
		_, ok := member(api, RoleGoMain, "example.com/shop/cmd/worker")
		assert.False(t, ok, "worker main in the api image")
	})

	t.Run("candidates are marked, never confirmed", func(t *testing.T) {
		_, worker := component(t, g, "worker")
		m, ok := member(worker, RoleWorkload, "CronJob/nightly container run")
		require.True(t, ok, "workload with a look-alike image")
		assert.True(t, m.Candidate)
		main, ok := member(worker, RoleGoMain, "example.com/shop/cmd/worker")
		require.True(t, ok)
		assert.False(t, main.Candidate, "the copy names /out/, which holds /out/worker")

		var inferred, syntactic int
		id := model.NodeID(topoRepo, model.KindComponent, "worker")
		for _, e := range g.Edges() {
			if e.From == id && e.Kind == model.EdgeAppliesTo {
				switch e.Evidence {
				case model.EvidenceInferred:
					inferred++
				case model.EvidenceSyntactic:
					syntactic++
				}
			}
		}
		assert.Equal(t, inferred, 1)
		assert.Greater(t, syntactic, 1)
	})

	t.Run("environment keys", func(t *testing.T) {
		_, api := component(t, g, "api")
		db, ok := envKey(api, "DATABASE_URL")
		require.True(t, ok)
		require.Len(t, db.ReadBy, 1)
		assert.Equal(t, db.ReadBy[0].File, "internal/conf/conf.go", "read through a constant, in an imported package")
		assert.Len(t, db.SetBy, 2) // compose and the Deployment
		_, ok = envKey(api, "LOG_LEVEL")
		assert.True(t, ok, "merged from a compose anchor")
		_, ok = envKey(api, "QUEUE")
		assert.False(t, ok, "the worker's key")
		_, worker := component(t, g, "worker")
		q, ok := envKey(worker, "QUEUE")
		require.True(t, ok)
		assert.Len(t, q.ReadBy, 1, "read through an aliased os import")
		assert.Len(t, q.SetBy, 1, "a list entry")
	})

	t.Run("no values reach the graph", func(t *testing.T) {
		for _, name := range []string{"api", "worker", "api-2"} {
			n, _ := component(t, g, name)
			for _, secret := range []string{"hunter2", "postgres://", "8080", "9090", "ghcr.io", "registry.example.com", "jobs", "info"} {
				assert.NotContains(t, n.Meta+n.Summary, secret, name)
			}
		}
	})

	t.Run("same name is never merged", func(t *testing.T) {
		n, top := component(t, g, "api-2")
		assert.Equal(t, n.File, "tools/api/Dockerfile")
		assert.Len(t, top.Members, 1)
	})

	t.Run("deterministic", func(t *testing.T) {
		g2 := topoGraph(t, root)
		require.NoError(t, BuildTopology(context.Background(), topoRepo, root, g2))
		for _, name := range []string{"api", "worker", "api-2"} {
			a, _ := component(t, g, name)
			b, _ := component(t, g2, name)
			assert.Equal(t, a.Meta, b.Meta, name)
		}
	})
}

// A record that names a component meets the topology on the same node, and
// the record's own name for it is kept.
func TestTopologyJoinsRecordComponent(t *testing.T) {
	root := writeTopoFixture(t)
	g := topoGraph(t, root)
	cid := model.NodeID(topoRepo, model.KindComponent, ComponentKey("API"))
	g.AddNode(model.Node{ID: cid, Kind: model.KindComponent, Repo: topoRepo, Name: "api", QualifiedName: "api"})
	require.NoError(t, BuildTopology(context.Background(), topoRepo, root, g))
	n, ok := g.Node(cid)
	require.True(t, ok)
	_, ok = TopologyOf(*n)
	assert.True(t, ok)
}

func TestGoBuilds(t *testing.T) {
	tests := []struct {
		args string
		want []string // dir=out
	}{
		{`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/a ./cmd/a`, []string{"cmd/a=/out/a"}},
		{`go build -o=/b ./x && go install ./y/`, []string{"x=/b", "y="}},
		{`go build ./...`, nil},
		{`go build -o /o $PKG`, nil},
		{`echo go`, nil},
		{`go test ./cmd/a`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			var got []string
			for _, b := range goBuilds(tt.args, 1) {
				got = append(got, b.dir+"="+b.out)
			}
			assert.Equal(t, strings.Join(got, ","), strings.Join(tt.want, ","))
		})
	}
}

func FuzzParseStages(f *testing.F) {
	f.Add(topoFixture["Dockerfile"])
	f.Add("FROM a AS b\nFROM b\nCOPY --from=b /x /y\nRUN go build -o /x ./c")
	f.Fuzz(func(t *testing.T, text string) {
		df := parseStages("Dockerfile", []byte(text))
		if len(df.stages) > maxStages {
			t.Fatalf("%d stages", len(df.stages))
		}
		for _, i := range df.targets() {
			if n := len(df.stageBuilds(i)); n > maxStageLinks {
				t.Fatalf("stage %d carries %d builds", i, n)
			}
		}
	})
}

func TestImageRefs(t *testing.T) {
	tests := []struct {
		ref, repo, base string
	}{
		{"ghcr.io/acme/api:1.0", "ghcr.io/acme/api", "api"},
		{"localhost:5000/api@sha256:abc", "localhost:5000/api", "api"},
		{"API", "api", "api"},
		{"{{ .Values.image }}", "", ""},
		{"${REGISTRY}/api:1", "", "api"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			repo, _ := imageRepo(tt.ref)
			assert.Equal(t, repo, tt.repo)
			assert.Equal(t, imageBase(tt.ref), tt.base)
		})
	}
}
