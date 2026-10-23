package golang

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// These fixtures record batch extraction coverage; retaining a declaration
// from broken code does not establish that its types or calls are complete.
func TestAuditPartialGoCoverage(t *testing.T) {
	for _, tt := range []struct {
		name, broken string
		wantBroken   bool
	}{
		{"complete", "func Broken() {}\n", true},
		{"unfinished body", "func Broken() {\n", true},
		{"unfinished declaration", "func Broken(\n", true},
		{"invalid package", "this is not Go\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			broken := "package audit\n" + tt.broken
			if tt.name == "invalid package" {
				broken = tt.broken
			}
			for name, source := range map[string]string{
				"go.mod":    "module example.com/audit\n\ngo 1.23\n",
				"valid.go":  "package audit\nfunc Retained() {}\n",
				"broken.go": broken,
			} {
				require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(source), 0o600))
			}
			ledger, err := ingestreport.New("audit", time.Now())
			require.NoError(t, err)
			ctx := ingestreport.WithLedger(context.Background(), ledger)
			require.NoError(t, ledger.Discover(ctx, root, nil))
			res, err := Extract(ctx, Options{Repo: "audit", Root: root})
			require.NoError(t, err)
			_, retained := res.Graph.Node(model.NodeID("audit", model.KindFunction, "example.com/audit.Retained"))
			_, partial := res.Graph.Node(model.NodeID("audit", model.KindFunction, "example.com/audit.Broken"))
			assert.True(t, retained, "valid declarations in the same package remain available")
			assert.Equal(t, partial, tt.wantBroken)
			observed := false
			for _, file := range ledger.Snapshot().Files {
				if file.Path != "valid.go" {
					continue
				}
				for _, stage := range file.Stages {
					if stage.Stage == ingestreport.Go {
						observed = true
						assert.Equal(t, stage.Outcome == ingestreport.StageFailed, tt.name != "complete", "package errors qualify even retained files")
					}
				}
			}
			assert.True(t, observed, "the ledger records Go extraction quality")
		})
	}
}

func TestAuditGoCallPrecision(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/audit\n\ngo 1.23\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "calls.go"), []byte(`package audit
func Target() {}
func Generic[T any](value T) T { return value }
func Direct() { Target(); Generic[int](1) }
func ThroughValue() { fn := Target; fn() }
type Runner interface { Run() }
type Used struct{}
func (Used) Run() {}
type Unused struct{}
func (Unused) Run() {}
func Dispatch(r Runner) { r.Run() }
func Entry() { Dispatch(Used{}) }
`), 0o600))
	res, err := Extract(context.Background(), Options{Repo: "audit", Root: root})
	require.NoError(t, err)
	id := func(kind model.NodeKind, name string) string { return model.NodeID("audit", kind, name) }
	for _, tt := range []struct {
		name, from, to string
		kind           model.EdgeKind
		want           bool
	}{
		{"direct", id(model.KindFunction, "example.com/audit.Direct"), id(model.KindFunction, "example.com/audit.Target"), model.EdgeCalls, true},
		{"generic", id(model.KindFunction, "example.com/audit.Direct"), id(model.KindFunction, "example.com/audit.Generic"), model.EdgeCalls, true},
		{"function value reference", id(model.KindFunction, "example.com/audit.ThroughValue"), id(model.KindFunction, "example.com/audit.Target"), model.EdgeReferences, true},
		{"function value call omitted", id(model.KindFunction, "example.com/audit.ThroughValue"), id(model.KindFunction, "example.com/audit.Target"), model.EdgeCalls, false},
		{"uninstantiated implementation remains a candidate", id(model.KindInterfaceMethod, "(example.com/audit.Runner).Run"), id(model.KindMethod, "(example.com/audit.Unused).Run"), model.EdgeDispatchesTo, true},
	} {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, hasEdge(res.Graph, tt.from, tt.to, tt.kind), tt.want) })
	}
}

func BenchmarkAuditGoExtract(b *testing.B) {
	for _, functions := range []int{10, 1000} {
		b.Run(fmt.Sprintf("functions_%d", functions), func(b *testing.B) {
			root := b.TempDir()
			var source strings.Builder
			source.WriteString("package audit\n")
			for i := 0; i < functions; i++ {
				fmt.Fprintf(&source, "func F%d(value int) int { if value < 0 { return -value }; return value + %d }\n", i, i)
			}
			require.NoError(b, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/audit\n\ngo 1.23\n"), 0o600))
			require.NoError(b, os.WriteFile(filepath.Join(root, "audit.go"), []byte(source.String()), 0o600))
			b.ReportAllocs()
			b.SetBytes(int64(source.Len()))
			for b.Loop() {
				res, err := Extract(context.Background(), Options{Repo: "audit", Root: root})
				require.NoError(b, err)
				require.Equal(b, len(res.Graph.Nodes()), functions+3)
			}
		})
	}
}
